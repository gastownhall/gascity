package beadmail

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail"
)

// Inbox is one read of the open message rows addressed to a known set of
// mailbox routes. Every inbox question a command asks about the same mailbox
// is a projection of it, so the command reads the mailbox once.
type Inbox struct {
	routes []string
	rows   []beads.Bead
}

// InboxForRoutes reads the open message rows addressed to exactly routes, in
// one listing. Unlike [Provider.Check], it does not expand the routes: the
// caller already holds the mailbox's whole route set — for a session, the
// [session.RecipientRoutesFromInfo] of its own bead, which is what expanding
// each of its addresses one by one arrives at. Empty routes are refused rather
// than read as "every mailbox".
func (p *Provider) InboxForRoutes(routes []string) (*Inbox, error) {
	var cleaned []string
	for _, route := range routes {
		cleaned = appendRecipientRoute(cleaned, route)
	}
	if len(cleaned) == 0 {
		return nil, fmt.Errorf("beadmail inbox: no mailbox routes")
	}
	candidates, err := p.messageCandidatesForRoutes(cleaned)
	if err != nil {
		return nil, fmt.Errorf("beadmail: listing beads: %w", err)
	}
	rows := make([]beads.Bead, 0, len(candidates))
	for _, b := range candidates {
		if b.Status != "open" || !matchesRecipientRoute(cleaned, b.Assignee) {
			continue
		}
		rows = append(rows, b)
	}
	return &Inbox{routes: cleaned, rows: rows}, nil
}

// Routes returns the mailbox routes the inbox was read for.
func (in *Inbox) Routes() []string {
	return append([]string(nil), in.routes...)
}

// Unread returns the inbox's unread messages, oldest first (ties by id) — the
// set and order [Provider.Check] yields for the same mailbox.
func (in *Inbox) Unread() []mail.Message {
	var messages []mail.Message
	for _, b := range in.rows {
		if hasLabel(b.Labels, "read") {
			continue
		}
		messages = append(messages, beadToMessage(b))
	}
	sortMessagesByArrival(messages)
	return messages
}

// ArchiveInjectedAutoHandoffsFromInbox is [Provider.ArchiveInjectedAutoHandoffs]
// over the rows inbox already read: of the injected ids, the auto-handoff mail
// that opted into archive-after-inject is marked read and closed without being
// read again. An id the inbox did not read is left alone — it was not injected
// from this inbox.
func (p *Provider) ArchiveInjectedAutoHandoffsFromInbox(inbox *Inbox, ids []string) error {
	if inbox == nil {
		return nil
	}
	rows := make(map[string]beads.Bead, len(inbox.rows))
	for _, b := range inbox.rows {
		rows[b.ID] = b
	}
	var errs []error
	for _, id := range ids {
		b, ok := rows[strings.TrimSpace(id)]
		if !ok || !isInjectedAutoHandoff(b) {
			continue
		}
		if err := p.archiveInjectedAutoHandoff(b.ID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// sortMessagesByArrival orders messages oldest first, ties by id.
func sortMessagesByArrival(messages []mail.Message) {
	sort.Slice(messages, func(i, j int) bool {
		if messages[i].CreatedAt.Equal(messages[j].CreatedAt) {
			return messages[i].ID < messages[j].ID
		}
		return messages[i].CreatedAt.Before(messages[j].CreatedAt)
	})
}
