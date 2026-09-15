// Package mail defines the pluggable mail provider interface for Gas City.
// The primary extension point is the exec script protocol (see
// internal/mail/exec); the Go interface exists for code organization and
// testability.
package mail //nolint:revive // internal package, always imported qualified

import (
	"errors"
	"time"
)

// ErrAlreadyArchived is returned by [Provider.Archive] when the message has
// already been archived or deleted. CLI code uses this to print a distinct
// message.
var ErrAlreadyArchived = errors.New("already archived")

// ErrNotFound is returned when a message ID does not exist.
var ErrNotFound = errors.New("message not found")

const (
	// AutoHandoffLabel marks mail created by gc handoff --auto for provider
	// context-cycle delivery.
	AutoHandoffLabel = "gc:auto-handoff"
	// ArchiveAfterInjectLabel marks system mail that should be archived after
	// successful hook injection.
	ArchiveAfterInjectLabel = "gc:archive-after-inject"
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
	// ReadMetadataKey mirrors the "read" label as a queryable metadata flag
	// ("true"/"false"), set alongside the label by MarkRead/MarkUnread. Retention
	// sweeps query it directly (the label-based query is recipient-scoped).
	ReadMetadataKey = "mail.read"
	// SupersedeKeyMetadataKey names the recurring order whose digest this
	// message carries. When a new message repeats the key, the same sender,
	// and the same recipient, the backend archives the earlier unread one, so
	// an hourly full-state digest keeps exactly one unread copy — the newest.
	// Only a recurring order sets it; a worker's one-off report never does.
	SupersedeKeyMetadataKey = "mail.supersede_key"
	// BlockedOnMetadataKey names the bead a BLOCKED report waits on. The
	// inbox sweep archives such a message once that bead closes, and never
	// while it is still open or in progress.
	BlockedOnMetadataKey = "mail.blocked_on"
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

// HandoffIntent is the domain-shaped request for handoff mail. It lets the
// gc handoff command express a message in mail terms — sender, recipient,
// subject, body — plus the two handoff-specific routing details that ordinary
// [Provider.Send] does not surface: an explicit thread ID (so a handoff thread
// is stable and addressable) and extra labels (the auto-handoff / archive-after-
// inject markers). The bead translation of these fields is confined to the
// backend implementation; callers never construct a message bead themselves.
type HandoffIntent struct {
	From        string
	To          string
	Subject     string
	Body        string
	ThreadID    string
	ExtraLabels []string
}

// ArchiveResult is one message's outcome in a batch [Provider.ArchiveMany] or
// [Provider.DeleteMany] call. Err is nil for a newly-archived/deleted message,
// [ErrAlreadyArchived] for an idempotent repeat, or a provider error.
type ArchiveResult struct {
	ID  string
	Err error
}

// Provider is the internal interface for mail backends and the canonical
// domain seam for mail: every mail caller speaks mail.Message (and
// [HandoffIntent]) here, never a raw message bead. The translation between mail
// and storage rows is confined to each backend — for the built-in default that
// edge is beadmail.Provider, where a message becomes a Type="message" bead.
// Implementations include beadmail (built-in default backed by beads.Store) and
// exec (user-supplied script via fork/exec).
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

	// Archive removes a message from all views. Bead-backed implementations
	// delete the message bead eagerly.
	Archive(id string) error

	// ArchiveMany archives a batch of messages in one round-trip where the
	// backend supports it, returning per-id results in input order.
	// Implementations MUST preserve per-id error reporting.
	ArchiveMany(ids []string) ([]ArchiveResult, error)

	// Delete is an alias for Archive.
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
	// The id may be either the thread ID or any message ID in that thread.
	Thread(id string) ([]Message, error)

	// All returns all open messages (read and unread) for the recipient.
	All(recipient string) ([]Message, error)

	// Count returns (total, unread) message counts for a recipient.
	Count(recipient string) (total int, unread int, err error)
}

// MultiRecipientInboxer is an optional extension for providers that can return
// unread inbox messages for multiple recipients in one backend pass.
type MultiRecipientInboxer interface {
	InboxRecipients(recipients []string) ([]Message, error)
}

// MetadataSender is an optional extension for providers that can attach
// caller metadata to a new message. gc mail send needs it for --supersede
// and --blocked-on; a backend without it rejects those flags rather than
// dropping the annotation silently.
//
// A provider that honors [SupersedeKeyMetadataKey] MUST archive the sender's
// earlier unread messages carrying the same key to the same recipient, and
// MUST leave every other message alone.
type MetadataSender interface {
	SendWithMetadata(from, to, subject, body string, metadata map[string]string) (Message, error)
}

// BlockerSweeper is an optional extension for providers that can retire a
// BLOCKED report whose blocker is gone. gc mail inbox calls it before it
// lists, so a resolved escalation leaves the inbox without a human reading
// each one.
type BlockerSweeper interface {
	// ArchiveResolvedBlockers archives unread messages for recipients whose
	// [BlockedOnMetadataKey] bead is closed, and returns the archived ids.
	ArchiveResolvedBlockers(recipients []string) ([]string, error)
}
