// Package beadmail implements [mail.Provider] backed by [beads.Store].
// This is the built-in default mail backend — messages are stored as beads
// with Type="message". No subprocess needed.
package beadmail

import (
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/session"
)

const (
	fromSessionIDMetadataKey = mail.FromSessionIDMetadataKey
	fromDisplayMetadataKey   = mail.FromDisplayMetadataKey
	toSessionIDMetadataKey   = mail.ToSessionIDMetadataKey
	toDisplayMetadataKey     = mail.ToDisplayMetadataKey
)

// Provider implements [mail.Provider] using [beads.Store] as the backend.
type Provider struct {
	store        beads.Store
	sessionCache *sessionBeadCache
}

type sessionBeadCache struct {
	mu      sync.Mutex
	list    []beads.Bead
	fetched bool
}

// New returns a beadmail provider backed by the given store.
//
// The default provider is stateless so long-lived shared users such as the API
// always see fresh session topology.
func New(store beads.Store) *Provider {
	return &Provider{store: store}
}

// NewCached returns a beadmail provider backed by the given store with a
// provider-local session enumeration cache. Command-scoped callers use this to
// avoid repeated session scans during one command. Long-lived API providers use
// it to keep steady-state mail reads cheap; they observe session-topology
// changes when their owning controller rebuilds the provider.
func NewCached(store beads.Store) *Provider {
	return &Provider{
		store:        store,
		sessionCache: &sessionBeadCache{},
	}
}

// cachedSessionBeads returns the full set of session beads (open + closed).
// Cached providers reuse a single enumeration; stateless providers fetch
// fresh results on every call.
func (p *Provider) cachedSessionBeads() ([]beads.Bead, error) {
	if p.store == nil {
		return nil, nil
	}
	if p.sessionCache == nil {
		return session.ListAllSessionBeads(p.store, beads.ListQuery{IncludeClosed: true})
	}
	return p.sessionCache.get(p.store)
}

func (c *sessionBeadCache) get(store beads.Store) ([]beads.Bead, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fetched {
		return c.list, nil
	}
	list, err := session.ListAllSessionBeads(store, beads.ListQuery{IncludeClosed: true})
	if err != nil {
		return nil, err
	}
	c.list = list
	c.fetched = true
	return list, nil
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

	b, err := p.store.Create(beads.Bead{
		Title:       title,
		Description: body,
		Type:        "message",
		Assignee:    to,
		From:        from,
		Labels:      labels,
		Metadata:    metadata,
		Ephemeral:   true,
	})
	if err != nil {
		return mail.Message{}, fmt.Errorf("beadmail send: %w", err)
	}
	return beadToMessage(b), nil
}

func (p *Provider) resolveSenderRoute(from string) (string, map[string]string, error) {
	from = strings.TrimSpace(from)
	if from == "" || from == "human" || p.store == nil {
		return from, nil, nil
	}
	sessionID, err := session.ResolveSessionID(p.store, from)
	if err != nil {
		if errors.Is(err, session.ErrSessionNotFound) || errors.Is(err, session.ErrAmbiguous) {
			return from, nil, nil
		}
		return "", nil, fmt.Errorf("resolving sender %q: %w", from, err)
	}
	b, err := p.store.Get(sessionID)
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
	if b.Type != "message" {
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
	if b.Type != "message" {
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
		Type:        "message",
		Assignee:    to, // reply goes back to sender
		From:        from,
		Labels:      labels,
		Metadata:    metadata,
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
		if msgBead.Type != "message" {
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
		Type:     "message",
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

// Recipient route helpers expand an operator-facing recipient into every
// stable mailbox address that might hold mail for that recipient.
func (p *Provider) recipientRoutes(recipient string) []string {
	recipient = strings.TrimSpace(recipient)
	if recipient == "" {
		return nil
	}
	routes := make([]string, 0, 4)
	routes = appendRecipientRoute(routes, recipient)
	if recipient == "human" || p.store == nil {
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
	b, err := p.store.Get(recipient)
	if err == nil && session.IsSessionBeadOrRepairable(b) && sessionRouteStatusMatches(b, closed) {
		session.RepairEmptyType(p.store, &b)
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
	query := beads.ListQuery{Metadata: map[string]string{key: recipient}}
	if status != "" {
		query.Status = status
	}
	items, err := p.store.List(query)
	if err != nil {
		return nil, err
	}
	matches := make([]beads.Bead, 0, len(items))
	for _, b := range items {
		if !session.IsSessionBeadOrRepairable(b) {
			continue
		}
		session.RepairEmptyType(p.store, &b)
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
// Empty routes return all open messages. bd stores message beads as wisps, but
// older stores can still have issue-tier messages, so mail reads must union
// both tiers.
func (p *Provider) messageCandidatesAll(routes []string) ([]beads.Bead, error) {
	all, err := p.store.List(beads.ListQuery{
		Type:      "message",
		Status:    "open",
		TierMode:  beads.TierBoth,
		AllowScan: true,
		Live:      true,
	})
	if err != nil {
		return nil, fmt.Errorf("scanning message beads: %w", err)
	}
	if len(routes) == 0 {
		return all, nil
	}
	out := make([]beads.Bead, 0, len(all))
	for _, b := range all {
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
