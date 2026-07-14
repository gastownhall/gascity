// Package beadmail implements [mail.Provider] backed by [beads.Store].
// This is the built-in default mail backend — messages are stored as beads
// with Type="message". No subprocess needed.
//
// beadmail is the confined bead/storage-row edge for mail: the mail.Message ⇄
// message-bead translation lives only here (createMessageBead, beadToMessage).
// Callers above this package speak mail.Message and never construct a message
// bead directly — see [mail.Provider] for the domain seam.
package beadmail

import (
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/session"
)

const (
	fromSessionIDMetadataKey = mail.FromSessionIDMetadataKey
	fromDisplayMetadataKey   = mail.FromDisplayMetadataKey
	toSessionIDMetadataKey   = mail.ToSessionIDMetadataKey
	toDisplayMetadataKey     = mail.ToDisplayMetadataKey

	// messageBeadType is the bead Type every mail message carries. It is the
	// single confined spelling of the message-bead class marker.
	messageBeadType = "message"

	cachedSessionBeadRefreshInterval = 30 * time.Second
)

// Provider implements [mail.Provider] using [beads.Store] as the backend.
//
// store persists message beads (messaging class); sessionStore serves the
// session-bead reads/writes mail uses for addressing and identity resolution
// (session class). They are the same work store at the single-store bd backend
// and diverge only once [beads.classes.sessions] relocates the session backend.
type Provider struct {
	store        beads.Store
	sessionStore beads.Store
	sessionCache *sessionBeadCache
}

type sessionBeadCache struct {
	mu              sync.Mutex
	list            []beads.Bead
	fetchedAt       time.Time
	refreshInterval time.Duration
	now             func() time.Time
	fetched         bool
}

// New returns a beadmail provider backed by the given store for both message
// persistence and session addressing. It is the single-store form of
// [NewWithStores].
//
// The default provider is stateless so long-lived shared users such as the API
// always see fresh session topology.
func New(store beads.Store) *Provider {
	return NewWithStores(store, store)
}

// NewWithStores returns a stateless beadmail provider whose message beads
// persist in msgStore (messaging class) and whose session reads/writes for mail
// addressing and identity resolution use sessionStore (session class). Pass the
// same store for both at the single-store bd backend; pass the relocated session
// store once [beads.classes.sessions] moves so mail addressing follows it.
func NewWithStores(msgStore, sessionStore beads.Store) *Provider {
	return &Provider{store: msgStore, sessionStore: sessionStore}
}

// NewCached returns a beadmail provider backed by the given store with a
// provider-local session enumeration cache. Command-scoped callers use this to
// avoid repeated session scans during one command. Long-lived API providers use
// it to keep steady-state mail reads cheap; they refresh session topology after
// a bounded interval so new and closed sessions are observed without controller
// restart. It is the single-store form of [NewCachedWithStores].
func NewCached(store beads.Store) *Provider {
	return NewCachedWithStores(store, store)
}

// NewCachedWithStores is the two-store form of [NewCached]: message persistence
// on msgStore, session addressing on sessionStore, with the provider-local
// session enumeration cache reading from sessionStore.
func NewCachedWithStores(msgStore, sessionStore beads.Store) *Provider {
	return &Provider{
		store:        msgStore,
		sessionStore: sessionStore,
		sessionCache: &sessionBeadCache{refreshInterval: cachedSessionBeadRefreshInterval},
	}
}

// cachedSessionBeads returns the full set of session beads (open + closed).
// Cached providers reuse a single enumeration; stateless providers fetch
// fresh results on every call.
func (p *Provider) cachedSessionBeads() ([]beads.Bead, error) {
	if p.sessionStore == nil {
		return nil, nil
	}
	if p.sessionCache == nil {
		return session.ListAllSessionBeads(p.sessionStore, beads.ListQuery{IncludeClosed: true})
	}
	return p.sessionCache.get(p.sessionStore)
}

func (c *sessionBeadCache) get(store beads.Store) ([]beads.Bead, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.currentTime()
	if c.fetched && c.isFresh(now) {
		return c.list, nil
	}
	list, err := session.ListAllSessionBeads(store, beads.ListQuery{IncludeClosed: true})
	if err != nil {
		return nil, err
	}
	c.list = list
	c.fetchedAt = now
	c.fetched = true
	return list, nil
}

func (c *sessionBeadCache) currentTime() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *sessionBeadCache) isFresh(now time.Time) bool {
	return c.refreshInterval > 0 && now.Sub(c.fetchedAt) < c.refreshInterval
}

// Send creates a message bead with subject in Title and body in Description.
// Returns an error if to is empty: blank recipients produce messages that never
// appear in any inbox but still inflate global counts.
func (p *Provider) Send(from, to, subject, body string) (mail.Message, error) {
	if to == "" {
		return mail.Message{}, fmt.Errorf("beadmail send: recipient is required")
	}
	from, metadata, err := p.resolveSenderRoute(from)
	if err != nil {
		return mail.Message{}, fmt.Errorf("beadmail send: %w", err)
	}
	threadID := generateThreadID()
	labels := []string{"thread:" + threadID}

	title := subject
	if title == "" && body != "" {
		title = strings.SplitN(body, "\n", 2)[0]
		if len(title) > 80 {
			title = title[:77] + "..."
		}
	}

	b, err := p.createMessageBead(title, body, from, to, labels, metadata)
	if err != nil {
		return mail.Message{}, fmt.Errorf("beadmail send: %w", err)
	}
	return beadToMessage(b), nil
}

// SendHandoff creates a handoff message from a [mail.HandoffIntent]. It speaks
// mail.Message at the boundary while confining the type=message bead, the
// stable thread label, and the handoff-specific extra labels to this
// implementation. Sender-route metadata is resolved exactly as [Provider.Send]
// does, so handoff mail replies route correctly.
func (p *Provider) SendHandoff(intent mail.HandoffIntent) (mail.Message, error) {
	if intent.To == "" {
		return mail.Message{}, fmt.Errorf("beadmail handoff: recipient is required")
	}
	from, metadata, err := p.resolveSenderRoute(intent.From)
	if err != nil {
		return mail.Message{}, fmt.Errorf("beadmail handoff: %w", err)
	}
	labels := make([]string, 0, 1+len(intent.ExtraLabels))
	labels = append(labels, "thread:"+intent.ThreadID)
	labels = append(labels, intent.ExtraLabels...)

	b, err := p.createMessageBead(intent.Subject, intent.Body, from, intent.To, labels, metadata)
	if err != nil {
		return mail.Message{}, fmt.Errorf("beadmail handoff: %w", err)
	}
	return beadToMessage(b), nil
}

// createMessageBead is the single confined edge where a mail message becomes a
// type=message bead. Every mail-creating method (Send, SendHandoff, Reply)
// funnels its already-resolved fields through here so the bead shape stays in
// one place.
func (p *Provider) createMessageBead(title, body, from, to string, labels []string, metadata map[string]string) (beads.Bead, error) {
	return p.store.Create(beads.Bead{
		Title:       title,
		Description: body,
		Type:        messageBeadType,
		Assignee:    to,
		From:        from,
		Labels:      labels,
		Metadata:    metadata,
		Ephemeral:   true,
	})
}

func (p *Provider) resolveSenderRoute(from string) (string, map[string]string, error) {
	from = strings.TrimSpace(from)
	if from == "" || from == "human" || p.sessionStore == nil {
		return from, nil, nil
	}
	sessionID, err := session.ResolveSessionID(p.sessionStore, from)
	if err != nil {
		if errors.Is(err, session.ErrSessionNotFound) || errors.Is(err, session.ErrAmbiguous) {
			return from, nil, nil
		}
		return "", nil, fmt.Errorf("resolving sender %q: %w", from, err)
	}
	b, err := p.sessionStore.Get(sessionID)
	if err != nil {
		return "", nil, fmt.Errorf("loading sender session %q: %w", sessionID, err)
	}
	display := senderDisplayAddress(b, from)
	metadata := map[string]string{fromSessionIDMetadataKey: sessionID}
	if display != "" {
		metadata[fromDisplayMetadataKey] = display
	}
	return display, metadata, nil
}

func senderDisplayAddress(b beads.Bead, fallback string) string {
	if alias := strings.TrimSpace(b.Metadata["alias"]); alias != "" {
		return alias
	}
	fallback = strings.TrimSpace(fallback)
	if fallback != "" && fallback != b.ID {
		return fallback
	}
	if name := strings.TrimSpace(b.Metadata["session_name"]); name != "" {
		return name
	}
	if b.ID != "" {
		return b.ID
	}
	return fallback
}

// Inbox returns all unread messages for the recipient.
func (p *Provider) Inbox(recipient string) ([]mail.Message, error) {
	return p.filterMessages(recipient, false)
}

// InboxRecipients returns all unread messages matching any recipient route in
// one message-bead scan.
func (p *Provider) InboxRecipients(recipients []string) ([]mail.Message, error) {
	return p.filterMessagesForRecipients(recipients, false)
}

// Get retrieves a message by ID without marking it read.
// Returns an error if the bead is not a message type.
func (p *Provider) Get(id string) (mail.Message, error) {
	b, err := p.store.Get(id)
	if err != nil {
		return mail.Message{}, fmt.Errorf("beadmail get: %w", err)
	}
	if b.Type != messageBeadType {
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
		if err := p.store.Update(id, beads.UpdateOpts{
			Labels:   []string{"read"},
			Metadata: map[string]string{"mail.read": "true"},
		}); err != nil {
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
	return p.store.Update(id, beads.UpdateOpts{
		Labels:   []string{"read"},
		Metadata: map[string]string{"mail.read": "true"},
	})
}

// MarkUnread marks a message as unread (removes "read" label).
func (p *Provider) MarkUnread(id string) error {
	if _, err := p.store.Get(id); err != nil {
		return fmt.Errorf("beadmail mark-unread: %w", err)
	}
	return p.store.Update(id, beads.UpdateOpts{
		RemoveLabels: []string{"read"},
		Metadata:     map[string]string{"mail.read": "false"},
	})
}

// ArchiveFilter selects open message beads for bounded archive cleanup.
type ArchiveFilter struct {
	Recipients      []string
	From            string
	SubjectPrefix   string
	SubjectContains string
	EmptyBody       bool
	IncludeRead     bool
	CaseInsensitive bool
	Limit           int
}

// Archive deletes a message bead without reading it.
func (p *Provider) Archive(id string) error {
	b, err := p.store.Get(id)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return mail.ErrAlreadyArchived
		}
		return fmt.Errorf("beadmail archive: %w", err)
	}
	if b.Type != messageBeadType {
		return fmt.Errorf("beadmail archive: bead %s is not a message", id)
	}
	if b.Status == "closed" {
		if err := p.store.Delete(id); err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				return mail.ErrAlreadyArchived
			}
			return fmt.Errorf("beadmail archive: %w", err)
		}
		return mail.ErrAlreadyArchived
	}
	if err := p.store.Delete(id); err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return mail.ErrAlreadyArchived
		}
		return fmt.Errorf("beadmail archive: %w", err)
	}
	return nil
}

// ArchiveCandidates returns open messages that match filter without archiving
// them.
func (p *Provider) ArchiveCandidates(filter ArchiveFilter) ([]mail.Message, error) {
	routes := p.recipientRoutesForAll(filter.Recipients)
	candidates, err := p.messageCandidatesForRoutes(routes)
	if err != nil {
		return nil, fmt.Errorf("beadmail archive matching: %w", err)
	}
	matches := make([]mail.Message, 0, len(candidates))
	for _, b := range candidates {
		if b.Status != "open" {
			continue
		}
		if len(routes) > 0 && !matchesRecipientRoute(routes, b.Assignee) {
			continue
		}
		msg := beadToMessage(b)
		if !filter.IncludeRead && msg.Read {
			continue
		}
		if !archiveExactMatches(msg.From, filter.From, filter.CaseInsensitive) {
			continue
		}
		if !archivePrefixMatches(msg.Subject, filter.SubjectPrefix, filter.CaseInsensitive) {
			continue
		}
		if !archiveContainsMatches(msg.Subject, filter.SubjectContains, filter.CaseInsensitive) {
			continue
		}
		if filter.EmptyBody && strings.TrimSpace(msg.Body) != "" {
			continue
		}
		matches = append(matches, msg)
		if filter.Limit > 0 && len(matches) >= filter.Limit {
			break
		}
	}
	return matches, nil
}

// ArchiveMatching deletes open messages selected by filter without per-message
// lookups after the candidate list has already verified them.
func (p *Provider) ArchiveMatching(filter ArchiveFilter) ([]mail.Message, []mail.ArchiveResult, error) {
	candidates, err := p.ArchiveCandidates(filter)
	if err != nil {
		return nil, nil, err
	}
	results := make([]mail.ArchiveResult, len(candidates))
	ids := make([]string, len(candidates))
	for i, msg := range candidates {
		ids[i] = msg.ID
		results[i] = mail.ArchiveResult{ID: msg.ID}
	}
	if len(ids) == 0 {
		return candidates, results, nil
	}
	for i, id := range ids {
		if err := p.store.Delete(id); err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				results[i].Err = mail.ErrAlreadyArchived
				continue
			}
			results[i].Err = fmt.Errorf("beadmail archive: %w", err)
		}
	}
	return candidates, results, nil
}

// ArchiveInjectedAutoHandoffs archives auto-handoff messages after they have
// been injected into a provider hook. Ordinary user mail is left untouched.
func (p *Provider) ArchiveInjectedAutoHandoffs(ids []string) error {
	var errs []error
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		b, err := p.store.Get(id)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			errs = append(errs, fmt.Errorf("loading %s: %w", id, err))
			continue
		}
		if b.Type != messageBeadType ||
			!hasLabel(b.Labels, mail.AutoHandoffLabel) ||
			!hasLabel(b.Labels, mail.ArchiveAfterInjectLabel) {
			continue
		}
		if err := p.store.Delete(id); err != nil && !errors.Is(err, beads.ErrNotFound) {
			errs = append(errs, fmt.Errorf("archiving %s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

func archiveExactMatches(value, exact string, insensitive bool) bool {
	exact = strings.TrimSpace(exact)
	if exact == "" {
		return true
	}
	if insensitive {
		value = strings.ToLower(value)
		exact = strings.ToLower(exact)
	}
	return value == exact
}

func archivePrefixMatches(value, prefix string, insensitive bool) bool {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return true
	}
	if insensitive {
		value = strings.ToLower(value)
		prefix = strings.ToLower(prefix)
	}
	return strings.HasPrefix(value, prefix)
}

func archiveContainsMatches(value, partial string, insensitive bool) bool {
	partial = strings.TrimSpace(partial)
	if partial == "" {
		return true
	}
	if insensitive {
		value = strings.ToLower(value)
		partial = strings.ToLower(partial)
	}
	return strings.Contains(value, partial)
}

// Delete is an alias for Archive.
func (p *Provider) Delete(id string) error {
	return p.Archive(id)
}

// ArchiveMany archives a batch of messages by deleting each bead eagerly,
// preserving per-id error reporting that matches [Provider.Archive].
func (p *Provider) ArchiveMany(ids []string) ([]mail.ArchiveResult, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	results := make([]mail.ArchiveResult, len(ids))
	for i, id := range ids {
		results[i] = mail.ArchiveResult{ID: id, Err: p.Archive(id)}
	}
	return results, nil
}

// DeleteMany deletes a batch of messages with the same storage semantics as
// [Provider.ArchiveMany].
func (p *Provider) DeleteMany(ids []string) ([]mail.ArchiveResult, error) {
	return p.ArchiveMany(ids)
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
	toSessionID := strings.TrimSpace(original.Metadata[fromSessionIDMetadataKey])
	to := toSessionID
	if to == "" {
		to = strings.TrimSpace(original.From)
	}
	if to == "" {
		return mail.Message{}, fmt.Errorf("beadmail reply: original message %s has no sender to reply to", id)
	}
	toDisplay := strings.TrimSpace(original.Metadata[fromDisplayMetadataKey])
	if toDisplay == "" {
		toDisplay = strings.TrimSpace(original.From)
	}
	from, metadata, err := p.resolveSenderRoute(from)
	if err != nil {
		return mail.Message{}, fmt.Errorf("beadmail reply: %w", err)
	}
	if metadata == nil {
		metadata = make(map[string]string)
	}
	if toSessionID != "" {
		metadata[toSessionIDMetadataKey] = toSessionID
	}
	if toDisplay != "" {
		metadata[toDisplayMetadataKey] = toDisplay
	}

	threadID := extractLabel(original.Labels, "thread:")
	if threadID == "" {
		threadID = generateThreadID()
	}

	labels := []string{"thread:" + threadID, "reply-to:" + id}

	b, err := p.store.Create(beads.Bead{
		Title:       deriveReplyTitle(subject, original.Title, body),
		Description: body,
		Type:        messageBeadType,
		Assignee:    to, // reply goes back to sender
		From:        from,
		Labels:      labels,
		Metadata:    metadata,
		Ephemeral:   true,
	})
	if err != nil {
		return mail.Message{}, fmt.Errorf("beadmail reply: %w", err)
	}
	return beadToMessage(b), nil
}

// deriveReplyTitle returns a non-empty title for a reply message. Callers
// that go through bd create fail validation ("title is required") if the
// reply's title is empty, so this fallback chain always returns a usable
// string. Precedence: explicit subject → "Re: <original>" (deduped) →
// first line of reply body → literal "(reply)".
func deriveReplyTitle(subject, originalTitle, body string) string {
	if subject != "" {
		return subject
	}
	if originalTitle != "" {
		trimmed := strings.TrimLeft(originalTitle, " \t")
		if strings.HasPrefix(strings.ToLower(trimmed), "re:") {
			return originalTitle
		}
		return "Re: " + originalTitle
	}
	snippet := strings.SplitN(body, "\n", 2)[0]
	if len(snippet) > 80 {
		snippet = snippet[:77] + "..."
	}
	if snippet != "" {
		return snippet
	}
	return "(reply)"
}

// Thread returns all messages sharing a thread ID, ordered by creation time.
// Callers may pass either an actual thread ID or any message bead ID in the
// thread — the latter is what `gc mail thread <id>` from the CLI hands us.
// If the input resolves to an existing message bead with a `thread:` label,
// that label is used; otherwise the input is treated as a thread ID directly
// so callers that already know the thread ID still work.
func (p *Provider) Thread(id string) ([]mail.Message, error) {
	threadID := id
	msgBead, err := p.store.Get(id)
	switch {
	case err == nil:
		if msgBead.Type != messageBeadType {
			return nil, fmt.Errorf("beadmail thread: bead %q is type %q, want message", id, msgBead.Type)
		}
		if t := extractLabel(msgBead.Labels, "thread:"); t != "" {
			threadID = t
		}
	case errors.Is(err, beads.ErrNotFound):
		// Caller passed a non-bead-id (e.g., a real thread-id); fall through.
	default:
		return nil, fmt.Errorf("beadmail thread: resolving %q: %w", id, err)
	}
	bs, err := p.store.List(beads.ListQuery{
		Label:    "thread:" + threadID,
		Type:     messageBeadType,
		Sort:     beads.SortCreatedAsc,
		TierMode: beads.TierBoth,
	})
	if err != nil {
		return nil, fmt.Errorf("beadmail thread: %w", err)
	}
	msgs := make([]mail.Message, len(bs))
	for i, b := range bs {
		msgs[i] = beadToMessage(b)
	}
	// Note: store.List already sorts by SortCreatedAsc with an ID tie-break
	// (see sortBeadsForQuery in internal/beads/query.go), so no post-sort here.
	return msgs, nil
}

// Count returns (total, unread) message counts for a recipient.
func (p *Provider) Count(recipient string) (int, int, error) {
	total, unread, err := p.CountRecipients([]string{recipient})
	if err != nil {
		return 0, 0, fmt.Errorf("beadmail count: %w", err)
	}
	return total, unread, nil
}

// CountRecipients returns deduplicated total and unread counts for all recipient
// routes represented by recipients.
func (p *Provider) CountRecipients(recipients []string) (int, int, error) {
	if len(recipients) == 0 {
		return 0, 0, nil
	}
	routes := p.recipientRoutesForAll(recipients)
	candidates, err := p.messageCandidatesForRoutes(routes)
	if err != nil {
		return 0, 0, fmt.Errorf("listing messages: %w", err)
	}
	var total, unread int
	for _, b := range candidates {
		if b.Status != "open" {
			continue
		}
		if len(routes) > 0 && !matchesRecipientRoute(routes, b.Assignee) {
			continue
		}
		total++
		if !hasLabel(b.Labels, "read") {
			unread++
		}
	}
	return total, unread, nil
}

// filterMessages returns open message beads assigned to the recipient.
// When includeRead is false, messages with the "read" label are excluded.
func (p *Provider) filterMessages(recipient string, includeRead bool) ([]mail.Message, error) {
	return p.filterMessagesForRecipients([]string{recipient}, includeRead)
}

// filterMessagesForRecipients returns open message beads assigned to any
// recipient route represented by recipients. Empty recipients mean all routes.
func (p *Provider) filterMessagesForRecipients(recipients []string, includeRead bool) ([]mail.Message, error) {
	routes := p.recipientRoutesForAll(recipients)
	candidates, err := p.messageCandidatesForRoutes(routes)
	if err != nil {
		return nil, fmt.Errorf("beadmail: listing beads: %w", err)
	}
	var msgs []mail.Message
	for _, b := range candidates {
		if b.Status != "open" {
			continue
		}
		if len(routes) > 0 && !matchesRecipientRoute(routes, b.Assignee) {
			continue
		}
		if !includeRead && hasLabel(b.Labels, "read") {
			continue
		}
		msgs = append(msgs, beadToMessage(b))
	}
	return msgs, nil
}

// IsMessageBead reports whether b is a mail message bead. It is the exported
// form of the message-bead class predicate so a caller that legitimately holds
// a raw bead from a cross-class graph walk (for example the order single-flight
// open-work gate) can test messaging membership without hardcoding the type
// literal. It is deliberately a bare Type check — NOT coordclass.Classify —
// because a message bead that also carries wisp metadata must still report true
// here, matching the historical inline test it replaces (coordclass.Classify
// would route such a bead to ClassGraph).
func IsMessageBead(b beads.Bead) bool {
	return b.Type == messageBeadType
}

// readMessagesBefore lists read message beads created before `before`, oldest
// first — the candidate set for the stale-mail retention sweep. The message-bead
// query shape (Type + "read" label) stays confined to this package, per the
// package invariant that callers above beadmail never construct a message-bead
// query directly. limit == 0 means unbounded.
func readMessagesBefore(store beads.Store, before time.Time, limit int) ([]beads.Bead, error) {
	return store.List(beads.ListQuery{
		Type:          messageBeadType,
		Label:         "read",
		CreatedBefore: before,
		Limit:         limit,
		Sort:          beads.SortCreatedAsc,
		TierMode:      beads.TierBoth,
	})
}

// SweepReadMessagesBefore closes read message beads created before cutoff,
// oldest first, stamping closeReason as "close_reason" metadata on each bead
// before closing it. It is the whole read-mail retention sweep: the candidate
// query and the close-with-reason loop live here because close_reason is
// bead-lifecycle vocabulary the mail.Message domain object deliberately omits,
// and because Provider.Archive/Provider.Delete mean eager delete — a different
// operation from close-with-reason.
//
// limit caps the number of beads closed (pass 0 for no cap); it bounds both the
// candidate query and the loop so a caller sharing a cross-phase close budget
// (see the nudge+mail sweep) honors it exactly. Beads that are no longer open
// when revisited are skipped without consuming the limit.
//
// Errors are split by severity so callers can preserve fatal-vs-recoverable
// handling: listErr is the fatal candidate-listing failure (no beads were
// swept), while closeErrs holds the per-bead metadata/close failures that do not
// abort the sweep. Returns the number of beads closed.
func SweepReadMessagesBefore(store beads.MailStore, cutoff time.Time, limit int, closeReason string) (closed int, closeErrs []error, listErr error) {
	candidates, err := readMessagesBefore(store.Store, cutoff, limit)
	if err != nil {
		return 0, nil, err
	}
	for _, b := range candidates {
		if limit > 0 && closed >= limit {
			break
		}
		if b.Status != "open" {
			continue
		}
		if err := store.SetMetadata(b.ID, "close_reason", closeReason); err != nil {
			closeErrs = append(closeErrs, fmt.Errorf("mail %s: set close_reason: %w", b.ID, err))
			continue
		}
		if err := store.Close(b.ID); err != nil {
			closeErrs = append(closeErrs, fmt.Errorf("mail %s: close: %w", b.ID, err))
			continue
		}
		closed++
	}
	return closed, closeErrs, nil
}

// CountReadMessagesBefore returns how many read message beads SweepReadMessagesBefore
// would close for the same cutoff and limit, without mutating any bead. It is the
// dry-run twin of the sweep and shares its candidate query and limit semantics so
// the two stay in lockstep.
func CountReadMessagesBefore(store beads.MailStore, cutoff time.Time, limit int) (int, error) {
	candidates, err := readMessagesBefore(store.Store, cutoff, limit)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, b := range candidates {
		if limit > 0 && count >= limit {
			break
		}
		if b.Status != "open" {
			continue
		}
		count++
	}
	return count, nil
}

// PurgeReadMessageWisps deletes read message beads in the wisp tier (open or
// closed) created before cutoff — the wisp-GC retention sweep for consumed mail.
// The candidate query and the delete loop live here because wisp-tier delete is
// bead-lifecycle behavior the mail.Message domain object omits. Each bead's
// dependencies are stripped before it is deleted (dependency-free single-row
// message beads make the strip a no-op in practice, but it preserves the
// retention delete semantics). Beads with a zero or not-yet-past CreatedAt are
// skipped. Per-bead delete failures are joined and returned without aborting the
// sweep; returns the number of beads purged.
func PurgeReadMessageWisps(store beads.MailStore, cutoff time.Time) (int, error) {
	entries, err := store.List(beads.ListQuery{
		Type:          messageBeadType,
		Metadata:      map[string]string{mail.ReadMetadataKey: "true"},
		IncludeClosed: true,
		TierMode:      beads.TierWisps,
	})
	if err != nil {
		return 0, fmt.Errorf("listing read message wisps: %w", err)
	}
	purged := 0
	var deleteErr error
	for _, entry := range entries {
		if entry.CreatedAt.IsZero() || !entry.CreatedAt.Before(cutoff) {
			continue
		}
		if err := deleteMessageWispBead(store.Store, entry.ID); err != nil {
			deleteErr = errors.Join(deleteErr, fmt.Errorf("deleting expired bead %q: %w", entry.ID, err))
			continue
		}
		purged++
	}
	return purged, deleteErr
}

// deleteMessageWispBead removes a message wisp bead, stripping its dependencies
// first, and restores any stripped dependency if a later step fails so a partial
// delete does not orphan the graph. It mirrors the wisp-tier delete semantics
// used by the shared graph GC.
func deleteMessageWispBead(store beads.Store, id string) error {
	downDeps, err := store.DepList(id, "down")
	if err != nil {
		return fmt.Errorf("list down deps: %w", err)
	}
	upDeps, err := store.DepList(id, "up")
	if err != nil {
		return fmt.Errorf("list up deps: %w", err)
	}
	removedDown := make([]beads.Dep, 0, len(downDeps))
	for _, dep := range downDeps {
		if err := store.DepRemove(id, dep.DependsOnID); err != nil {
			return withMessageWispDeleteRestore(
				fmt.Errorf("remove down dep %s -> %s: %w", id, dep.DependsOnID, err),
				restoreMessageWispDeps(store, removedDown, nil),
			)
		}
		removedDown = append(removedDown, dep)
	}
	removedUp := make([]beads.Dep, 0, len(upDeps))
	for _, dep := range upDeps {
		if err := store.DepRemove(dep.IssueID, id); err != nil {
			return withMessageWispDeleteRestore(
				fmt.Errorf("remove up dep %s -> %s: %w", dep.IssueID, id, err),
				restoreMessageWispDeps(store, removedDown, removedUp),
			)
		}
		removedUp = append(removedUp, dep)
	}
	if err := store.Delete(id); err != nil {
		return withMessageWispDeleteRestore(
			fmt.Errorf("delete bead: %w", err),
			restoreMessageWispDeps(store, removedDown, removedUp),
		)
	}
	return nil
}

func withMessageWispDeleteRestore(primary, restoreErr error) error {
	if restoreErr == nil {
		return primary
	}
	return errors.Join(primary, fmt.Errorf("rollback failed: %w", restoreErr))
}

func restoreMessageWispDeps(store beads.Store, downDeps, upDeps []beads.Dep) error {
	var restoreErr error
	for _, dep := range downDeps {
		if err := store.DepAdd(dep.IssueID, dep.DependsOnID, dep.Type); err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("restore dep %s -> %s: %w", dep.IssueID, dep.DependsOnID, err))
		}
	}
	for _, dep := range upDeps {
		if err := store.DepAdd(dep.IssueID, dep.DependsOnID, dep.Type); err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("restore dep %s -> %s: %w", dep.IssueID, dep.DependsOnID, err))
		}
	}
	return restoreErr
}

// Recipient route helpers expand an operator-facing recipient into every
// stable mailbox address that might hold mail for that recipient.
func (p *Provider) recipientRoutes(recipient string) []string {
	recipient = strings.TrimSpace(recipient)
	if recipient == "" {
		return nil
	}
	routes := make([]string, 0, 4)
	routes = appendRecipientRoute(routes, recipient)
	if recipient == "human" || p.sessionStore == nil {
		return routes
	}

	liveMatches, err := p.recipientSessionMatchesByCurrentAddress(recipient, false)
	if err != nil {
		log.Printf("beadmail: listing sessions for recipient route %q: %v", recipient, err)
		return routes
	}
	if len(liveMatches) > 1 {
		return []string{recipient}
	}
	if len(liveMatches) == 1 {
		return appendSessionRecipientRoutes(routes, liveMatches[0])
	}

	closedMatches, err := p.recipientSessionMatchesByCurrentAddress(recipient, true)
	if err != nil {
		log.Printf("beadmail: listing closed sessions for recipient route %q: %v", recipient, err)
		return routes
	}
	if len(closedMatches) > 1 {
		return []string{recipient}
	}
	if len(closedMatches) == 1 {
		return appendSessionRecipientRoutes(routes, closedMatches[0])
	}
	return p.recipientRoutesByHistoricalAlias(recipient, routes)
}

func (p *Provider) recipientSessionMatchesByCurrentAddress(recipient string, closed bool) ([]beads.Bead, error) {
	var matches []beads.Bead
	b, err := p.sessionStore.Get(recipient)
	if err == nil && session.IsSessionBeadOrRepairable(b) && sessionRouteStatusMatches(b, closed) {
		session.RepairEmptyType(p.sessionStore, &b)
		matches = appendUniqueSessionRecipientMatch(matches, b)
	} else if err != nil && !errors.Is(err, beads.ErrNotFound) {
		return nil, fmt.Errorf("looking up session %q: %w", recipient, err)
	}

	status := ""
	if closed {
		status = "closed"
	}
	for _, key := range []string{"alias", "session_name"} {
		keyMatches, err := p.recipientSessionMatchesByMetadata(key, recipient, status)
		if err != nil {
			return nil, err
		}
		for _, match := range keyMatches {
			matches = appendUniqueSessionRecipientMatch(matches, match)
		}
	}
	return matches, nil
}

func (p *Provider) recipientSessionMatchesByMetadata(key, recipient, status string) ([]beads.Bead, error) {
	query := beads.ListQuery{
		Metadata: map[string]string{key: recipient},
		TierMode: beads.TierBoth,
	}
	if status != "" {
		query.Status = status
	}
	items, err := p.sessionStore.List(query)
	if err != nil {
		return nil, err
	}
	matches := make([]beads.Bead, 0, len(items))
	for _, b := range items {
		if !session.IsSessionBeadOrRepairable(b) {
			continue
		}
		session.RepairEmptyType(p.sessionStore, &b)
		if !sessionRouteStatusMatches(b, status == "closed") {
			continue
		}
		if strings.TrimSpace(b.Metadata[key]) != recipient {
			continue
		}
		matches = append(matches, b)
	}
	return matches, nil
}

func sessionRouteStatusMatches(b beads.Bead, closed bool) bool {
	if closed {
		return b.Status == "closed"
	}
	return b.Status != "closed"
}

func appendUniqueSessionRecipientMatch(matches []beads.Bead, b beads.Bead) []beads.Bead {
	for _, match := range matches {
		if match.ID == b.ID {
			return matches
		}
	}
	return append(matches, b)
}

func appendSessionRecipientRoutes(routes []string, b beads.Bead) []string {
	for _, address := range sessionAddressesForRecipientRouting(b) {
		routes = appendRecipientRoute(routes, address)
	}
	return routes
}

func (p *Provider) recipientRoutesByHistoricalAlias(recipient string, routes []string) []string {
	sessions, err := p.cachedSessionBeads()
	if err != nil {
		log.Printf("beadmail: listing sessions for historical recipient route %q: %v", recipient, err)
		return routes
	}
	var liveMatches []beads.Bead
	var closedMatches []beads.Bead
	for _, b := range sessions {
		if !session.IsSessionBeadOrRepairable(b) || !containsRecipientRoute(session.AliasHistory(b.Metadata), recipient) {
			continue
		}
		if b.Status == "closed" {
			closedMatches = append(closedMatches, b)
			continue
		}
		liveMatches = append(liveMatches, b)
	}
	matches := liveMatches
	if len(matches) == 0 {
		matches = closedMatches
	}
	if len(matches) > 1 {
		return []string{recipient}
	}
	if len(matches) == 1 {
		return appendSessionRecipientRoutes(routes, matches[0])
	}
	return routes
}

func (p *Provider) recipientRoutesForAll(recipients []string) []string {
	var routes []string
	for _, recipient := range recipients {
		recipientRoutes := p.recipientRoutes(recipient)
		for _, route := range recipientRoutes {
			routes = appendRecipientRoute(routes, route)
		}
	}
	return routes
}

func sessionAddressesForRecipientRouting(b beads.Bead) []string {
	var routes []string
	routes = appendRecipientRoute(routes, b.ID)
	routes = appendRecipientRoute(routes, b.Metadata["alias"])
	routes = appendRecipientRoute(routes, b.Metadata["session_name"])
	for _, alias := range session.AliasHistory(b.Metadata) {
		routes = appendRecipientRoute(routes, alias)
	}
	return routes
}

func appendRecipientRoute(routes []string, route string) []string {
	route = strings.TrimSpace(route)
	if route == "" || containsRecipientRoute(routes, route) {
		return routes
	}
	return append(routes, route)
}

func containsRecipientRoute(routes []string, route string) bool {
	route = strings.TrimSpace(route)
	for _, candidate := range routes {
		if candidate == route {
			return true
		}
	}
	return false
}

func matchesRecipientRoute(routes []string, assignee string) bool {
	for _, route := range routes {
		if assignee == route {
			return true
		}
	}
	return false
}

func (p *Provider) messageCandidatesForRoutes(routes []string) ([]beads.Bead, error) {
	return p.messageCandidatesAll(routes)
}

// messageCandidatesAll returns all open message beads matching any route.
// TierBoth is one logical query; BdStore may satisfy it with separate
// issue-tier and wisp-tier reads before deduping. Empty routes return all open
// messages. Live reads are required so command-visible mail sees fresh wisps
// even when the active store cache was primed earlier.
func (p *Provider) messageCandidatesAll(routes []string) ([]beads.Bead, error) {
	query := beads.ListQuery{
		Type:     messageBeadType,
		Status:   "open",
		TierMode: beads.TierBoth,
		Live:     true,
	}
	if len(routes) > 0 {
		query.Assignees = routes
	} else {
		query.AllowScan = true
	}
	all, err := p.store.List(query)
	if err != nil {
		return nil, fmt.Errorf("scanning message beads: %w", err)
	}
	if len(routes) == 0 {
		return all, nil
	}
	out := make([]beads.Bead, 0, len(all))
	for _, b := range all {
		// matchesRecipientRoute is defense-in-depth: HQStore returns exact
		// matches from the index; BdStore multi-route fallback may return excess.
		if matchesRecipientRoute(routes, b.Assignee) {
			out = append(out, b)
		}
	}
	return out, nil
}

// beadToMessage converts a bead to a mail.Message.
func beadToMessage(b beads.Bead) mail.Message {
	from := b.From
	if display := strings.TrimSpace(b.Metadata[fromDisplayMetadataKey]); display != "" {
		from = display
	}
	to := b.Assignee
	if display := strings.TrimSpace(b.Metadata[toDisplayMetadataKey]); display != "" {
		to = display
	}
	read := hasLabel(b.Labels, "read")
	switch b.Metadata["mail.read"] {
	case "true":
		read = true
	case "false":
		read = false
	}
	return mail.Message{
		ID:        b.ID,
		From:      from,
		To:        to,
		Subject:   b.Title,
		Body:      b.Description,
		CreatedAt: b.CreatedAt,
		Read:      read,
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
