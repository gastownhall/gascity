// Package beadmail implements [mail.Provider] backed by [beads.Store].
// This is the built-in default mail backend — messages are stored as beads
// with Type="message". No subprocess needed.
package beadmail

import (
	"crypto/rand"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail"
)

// Provider implements [mail.Provider] using [beads.Store] as the backend.
type Provider struct {
	store beads.Store
}

// New returns a beadmail provider backed by the given store.
func New(store beads.Store) *Provider {
	return &Provider{store: store}
}

// Send creates a message bead with subject in Title and body in Description.
func (p *Provider) Send(from, to, subject, body string) (mail.Message, error) {
	threadID := generateThreadID()
	labels := []string{"gc:message", "thread:" + threadID}

	b, err := p.store.Create(beads.Bead{
		Title:       subject,
		Description: body,
		Type:        "message",
		Assignee:    to,
		From:        from,
		Labels:      labels,
	})
	if err != nil {
		return mail.Message{}, fmt.Errorf("beadmail send: %w", err)
	}
	return beadToMessage(b), nil
}

// Inbox returns all unread messages for the recipient.
func (p *Provider) Inbox(recipient string) ([]mail.Message, error) {
	return p.filterMessages(recipient, false)
}

// Get retrieves a message by ID without marking it read.
// Returns an error if the bead is not a message type.
func (p *Provider) Get(id string) (mail.Message, error) {
	b, err := p.store.Get(id)
	if err != nil {
		return mail.Message{}, fmt.Errorf("beadmail get: %w", err)
	}
	if b.Type != "" && b.Type != "message" {
		return mail.Message{}, fmt.Errorf("beadmail get: bead %s is type %q, not message", id, b.Type)
	}
	return beadToMessage(b), nil
}

// Read retrieves a message by ID and marks it as read (adds "read" label).
// The message remains in the store (not closed).
func (p *Provider) Read(id string) (mail.Message, error) {
	b, err := p.store.Get(id)
	if err != nil {
		return mail.Message{}, fmt.Errorf("beadmail read: %w", err)
	}
	if !hasLabel(b.Labels, "read") {
		if err := p.store.Update(id, beads.UpdateOpts{Labels: []string{"read"}}); err != nil {
			return mail.Message{}, fmt.Errorf("beadmail read: marking as read: %w", err)
		}
	}
	msg := beadToMessage(b)
	msg.Read = true
	return msg, nil
}

// MarkRead marks a message as read (adds "read" label).
func (p *Provider) MarkRead(id string) error {
	if _, err := p.store.Get(id); err != nil {
		return fmt.Errorf("beadmail mark-read: %w", err)
	}
	return p.store.Update(id, beads.UpdateOpts{Labels: []string{"read"}})
}

// MarkUnread marks a message as unread (removes "read" label).
func (p *Provider) MarkUnread(id string) error {
	if _, err := p.store.Get(id); err != nil {
		return fmt.Errorf("beadmail mark-unread: %w", err)
	}
	return p.store.Update(id, beads.UpdateOpts{RemoveLabels: []string{"read"}})
}

// Archive closes a message bead without reading it.
func (p *Provider) Archive(id string) error {
	b, err := p.store.Get(id)
	if err != nil {
		return fmt.Errorf("beadmail archive: %w", err)
	}
	if b.Type != "message" {
		return fmt.Errorf("beadmail archive: bead %s is not a message", id)
	}
	if b.Status == "closed" {
		return mail.ErrAlreadyArchived
	}
	if err := p.store.Close(id); err != nil {
		return fmt.Errorf("beadmail archive: %w", err)
	}
	return nil
}

// Delete is an alias for Archive (closes the bead).
func (p *Provider) Delete(id string) error {
	return p.Archive(id)
}

// All returns all open messages (read and unread) for the recipient.
func (p *Provider) All(recipient string) ([]mail.Message, error) {
	return p.filterMessages(recipient, true)
}

// Check returns unread messages for the recipient without marking them read.
func (p *Provider) Check(recipient string) ([]mail.Message, error) {
	return p.filterMessages(recipient, false)
}

// Reply creates a reply to an existing message. Inherits ThreadID from the
// original, sets ReplyTo to the original's ID. Reply is addressed to the
// original sender.
func (p *Provider) Reply(id, from, subject, body string) (mail.Message, error) {
	original, err := p.store.Get(id)
	if err != nil {
		return mail.Message{}, fmt.Errorf("beadmail reply: %w", err)
	}

	threadID := extractLabel(original.Labels, "thread:")
	if threadID == "" {
		threadID = generateThreadID()
	}

	labels := []string{"gc:message", "thread:" + threadID, "reply-to:" + id}

	b, err := p.store.Create(beads.Bead{
		Title:       subject,
		Description: body,
		Type:        "message",
		Assignee:    original.From, // reply goes back to sender
		From:        from,
		Labels:      labels,
	})
	if err != nil {
		return mail.Message{}, fmt.Errorf("beadmail reply: %w", err)
	}
	return beadToMessage(b), nil
}

// Thread returns all messages sharing a thread ID, ordered by creation time.
func (p *Provider) Thread(threadID string) ([]mail.Message, error) {
	bs, err := p.store.ListByLabel("thread:"+threadID, 0)
	if err != nil {
		return nil, fmt.Errorf("beadmail thread: %w", err)
	}
	msgs := make([]mail.Message, len(bs))
	for i, b := range bs {
		msgs[i] = beadToMessage(b)
	}
	// Sort by creation time ascending.
	sort.Slice(msgs, func(i, j int) bool {
		return msgs[i].CreatedAt.Before(msgs[j].CreatedAt)
	})
	return msgs, nil
}

// Count returns (total, unread) message counts for a recipient.
func (p *Provider) Count(recipient string) (int, int, error) {
	all, err := p.listMessages()
	if err != nil {
		return 0, 0, fmt.Errorf("beadmail count: %w", err)
	}
	var total, unread int
	for _, b := range all {
		if b.Status == "open" && (recipient == "" || b.Assignee == recipient) {
			total++
			if !hasLabel(b.Labels, "read") {
				unread++
			}
		}
	}
	return total, unread, nil
}

// filterMessages returns open message beads assigned to the recipient.
// When includeRead is false, messages with the "read" label are excluded.
func (p *Provider) filterMessages(recipient string, includeRead bool) ([]mail.Message, error) {
	all, err := p.listMessages()
	if err != nil {
		return nil, fmt.Errorf("beadmail: listing beads: %w", err)
	}
	var msgs []mail.Message
	for _, b := range all {
		if b.Status == "open" && (recipient == "" || b.Assignee == recipient) {
			if !includeRead && hasLabel(b.Labels, "read") {
				continue
			}
			msgs = append(msgs, beadToMessage(b))
		}
	}
	return msgs, nil
}

// listMessages returns message beads by combining the store's generic list with
// an explicit gc:message label query. Some external stores can retrieve message
// beads by ID and label query but omit them from the generic list output.
func (p *Provider) listMessages() ([]beads.Bead, error) {
	all, err := p.store.ListOpen()
	if err != nil {
		return nil, fmt.Errorf("listing beads: %w", err)
	}
	labeled, err := p.store.ListByLabel("gc:message", 0)
	if err != nil {
		return nil, fmt.Errorf("listing gc:message beads: %w", err)
	}

	seen := make(map[string]beads.Bead, len(all)+len(labeled))
	order := make([]string, 0, len(all)+len(labeled))
	add := func(bs []beads.Bead) {
		for _, b := range bs {
			if !isMessage(b) {
				continue
			}
			if _, ok := seen[b.ID]; !ok {
				order = append(order, b.ID)
			}
			seen[b.ID] = b
		}
	}
	add(all)
	add(labeled)

	result := make([]beads.Bead, 0, len(order))
	for _, id := range order {
		result = append(result, seen[id])
	}
	return result, nil
}

// isMessage returns true if the bead is a message (by Type or gc:message label).
func isMessage(b beads.Bead) bool {
	return b.Type == "message" || hasLabel(b.Labels, "gc:message")
}

// beadToMessage converts a bead to a mail.Message.
func beadToMessage(b beads.Bead) mail.Message {
	return mail.Message{
		ID:        b.ID,
		From:      b.From,
		To:        b.Assignee,
		Subject:   b.Title,
		Body:      b.Description,
		CreatedAt: b.CreatedAt,
		Read:      hasLabel(b.Labels, "read"),
		ThreadID:  extractLabel(b.Labels, "thread:"),
		ReplyTo:   extractLabel(b.Labels, "reply-to:"),
		Priority:  extractPriority(b.Labels),
		CC:        extractCC(b.Labels),
	}
}

// hasLabel reports whether labels contains the target string.
func hasLabel(labels []string, target string) bool {
	for _, l := range labels {
		if l == target {
			return true
		}
	}
	return false
}

// extractLabel returns the value after the prefix from the first matching
// label, or "" if none match. E.g. "thread:abc" with prefix "thread:" → "abc".
func extractLabel(labels []string, prefix string) string {
	for _, l := range labels {
		if strings.HasPrefix(l, prefix) {
			return l[len(prefix):]
		}
	}
	return ""
}

// extractPriority parses a "priority:N" label, returning 0 if not found.
func extractPriority(labels []string) int {
	s := extractLabel(labels, "priority:")
	if s == "" {
		return 0
	}
	n, _ := strconv.Atoi(s)
	return n
}

// extractCC extracts CC recipients from "cc:<addr>" labels.
func extractCC(labels []string) []string {
	var result []string
	for _, l := range labels {
		if strings.HasPrefix(l, "cc:") {
			result = append(result, l[3:])
		}
	}
	return result
}

// generateThreadID returns a unique thread identifier.
func generateThreadID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		// Fallback: should never happen.
		return "thread-fallback"
	}
	return fmt.Sprintf("thread-%x", b)
}

// Compile-time interface check.
var _ mail.Provider = (*Provider)(nil)
