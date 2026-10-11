package beadmail

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/session"
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
// caller already holds the route set it means. A session's own mailbox is read
// with [Provider.InboxForSession], which defines that set. Empty routes are
// refused rather than read as "every mailbox".
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

// InboxForSession reads one session's own mailbox in one message listing. It
// is the inbox every self-read — a hook's mail check and its SessionStart
// mail, and `gc mail inbox` — answers from, so they all see the same mail.
//
// The mailbox is the session's own routes ([session.RecipientRoutesFromInfo]:
// id, alias, session name and alias history) and, for each alias the session
// held before, the routes of its closed predecessor: the one closed session
// that holds that alias as a current address, when no live session claims it.
// Mail routed to that predecessor's id or runtime name is the session's to
// read; it was addressed to the alias the session now inherits. Two rules keep
// other mailboxes out, as delivery does: a live session claiming the alias
// owns its own routes, so they never join; and an alias two closed sessions
// claim is ambiguous, so neither's routes join. The alias itself is always a
// route.
//
// A session with no alias history reads no session at all. Each alias it held
// costs at most two keyed mailbox resolutions (live, then closed —
// [session.AddressDirectory.ResolveMailboxAddress]), never a listing of every
// session.
func (p *Provider) InboxForSession(info session.Info) (*Inbox, error) {
	routes, err := p.sessionInboxRoutes(info)
	if err != nil {
		return nil, err
	}
	return p.InboxForRoutes(routes)
}

// sessionInboxRoutes is the route set InboxForSession reads.
func (p *Provider) sessionInboxRoutes(info session.Info) ([]string, error) {
	routes := session.RecipientRoutesFromInfo(info)
	if p.sessions == nil {
		return routes, nil
	}
	current := []string{info.ID, info.Alias, info.SessionNameMetadata}
	for _, alias := range info.AliasHistory {
		alias = strings.TrimSpace(alias)
		if alias == "" || containsRecipientRoute(current, alias) {
			continue
		}
		predecessor, ok, err := p.closedPredecessor(alias, info.ID)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		for _, route := range session.RecipientRoutesFromInfo(predecessor) {
			routes = appendRecipientRoute(routes, route)
		}
	}
	return routes, nil
}

// closedPredecessor resolves the closed session that owns alias as a mailbox
// address, when exactly one does and no live session — other than self —
// claims it.
func (p *Provider) closedPredecessor(alias, self string) (session.Info, bool, error) {
	_, err := p.sessions.ResolveMailboxAddress(alias, false)
	switch {
	case err == nil:
		// A live claimant owns the alias now; it is no predecessor's.
		return session.Info{}, false, nil
	case errors.Is(err, session.ErrAmbiguous):
		return session.Info{}, false, nil
	case !errors.Is(err, session.ErrSessionNotFound):
		return session.Info{}, false, fmt.Errorf("beadmail inbox: resolving the live owner of alias %q: %w", alias, err)
	}
	closed, err := p.sessions.ResolveMailboxAddress(alias, true)
	switch {
	case err == nil:
		if closed.ID == self {
			return session.Info{}, false, nil
		}
		return closed, true, nil
	case errors.Is(err, session.ErrAmbiguous), errors.Is(err, session.ErrSessionNotFound):
		return session.Info{}, false, nil
	default:
		return session.Info{}, false, fmt.Errorf("beadmail inbox: resolving the closed owner of alias %q: %w", alias, err)
	}
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

// UnreadAutoHandoffs returns the inbox's unread auto-handoff mail that opted
// into archive-after-inject, oldest first (ties by id) — the set and order
// [Provider.CheckAutoHandoffs] yields for the same mailbox.
func (in *Inbox) UnreadAutoHandoffs() []mail.Message {
	var messages []mail.Message
	for _, b := range in.rows {
		if hasLabel(b.Labels, "read") || !isInjectedAutoHandoff(b) {
			continue
		}
		messages = append(messages, beadToMessage(b))
	}
	sortMessagesByArrival(messages)
	return messages
}

// InjectedAutoHandoffIDs returns, of ids, those the inbox read as unread
// auto-handoff mail that opted into archive-after-inject — exactly what
// [Provider.ArchiveInjectedAutoHandoffsFromInbox] archives — in ids' order. An
// id the inbox did not read is not one of them.
func (in *Inbox) InjectedAutoHandoffIDs(ids []string) []string {
	if in == nil {
		return nil
	}
	rows := make(map[string]beads.Bead, len(in.rows))
	for _, b := range in.rows {
		rows[b.ID] = b
	}
	var out []string
	for _, id := range ids {
		b, ok := rows[strings.TrimSpace(id)]
		if !ok || hasLabel(b.Labels, "read") || !isInjectedAutoHandoff(b) {
			continue
		}
		out = append(out, b.ID)
	}
	return out
}

// ArchiveInjectedAutoHandoffsFromInbox is [Provider.ArchiveInjectedAutoHandoffs]
// over the rows inbox already read: of the injected ids, the auto-handoff mail
// that opted into archive-after-inject ([Inbox.InjectedAutoHandoffIDs]) is
// marked read and closed without being read again. An id the inbox did not
// read is left alone — it was not injected from this inbox.
func (p *Provider) ArchiveInjectedAutoHandoffsFromInbox(inbox *Inbox, ids []string) error {
	var errs []error
	for _, id := range inbox.InjectedAutoHandoffIDs(ids) {
		if err := p.archiveInjectedAutoHandoff(id); err != nil {
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
