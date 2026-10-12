package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	"github.com/gastownhall/gascity/internal/session"
)

var errMailNoBeadStore = errors.New("no bead store available")

// findMailProvider returns the mail provider for a rig, or the first available
// (deterministically by sorted rig name).
func (s *Server) findMailProvider(rig string) mail.Provider {
	if rig != "" {
		return s.state.MailProvider(rig)
	}
	providers := s.state.MailProviders()
	names := sortedProviderNames(providers)
	if len(names) == 0 {
		return nil
	}
	return providers[names[0]]
}

// findMailProviderForMessage locates the mail provider and rig that own `id`.
// When `rigHint` is non-empty, it checks that provider first for an O(1)
// lookup instead of scanning all providers. Falls back to brute-force
// search if the hint misses (message moved/deleted from that rig).
func (s *Server) findMailProviderForMessage(id, rigHint string) (mail.Provider, string, error) {
	if rigHint != "" {
		if mp := s.state.MailProvider(rigHint); mp != nil {
			if _, err := mp.Get(id); err == nil {
				return mp, rigHint, nil
			} else if !errors.Is(err, mail.ErrNotFound) && !errors.Is(err, beads.ErrNotFound) {
				return nil, "", err
			}
		}
		// Hint missed — fall through to full scan.
	}
	return s.findMailProviderByID(id)
}

// findMailProviderByID searches all mail providers for one that contains the given message ID.
// Returns the provider and rig that own the message, or nil/""
// with an error if a provider failed.
// Returns (nil, "", nil) only when all providers definitively return ErrNotFound.
func (s *Server) findMailProviderByID(id string) (mail.Provider, string, error) {
	providers := s.state.MailProviders()
	var firstErr error
	for _, name := range sortedProviderNames(providers) {
		mp := providers[name]
		if _, err := mp.Get(id); err == nil {
			return mp, name, nil
		} else if !errors.Is(err, mail.ErrNotFound) && !errors.Is(err, beads.ErrNotFound) {
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return nil, "", firstErr
}

func (s *Server) resolveMailSendRecipientWithContext(ctx context.Context, recipient string) (string, error) {
	recipient = strings.TrimSpace(recipient)
	if recipient == "human" {
		return recipient, nil
	}
	store := s.state.SessionsBeadStore().Store
	if store == nil {
		resolved, err := mail.ResolveRecipient(recipient, agentEntries(s.state.Config()))
		if err != nil {
			return "", errMailNoBeadStore
		}
		return resolved, nil
	}
	if target, matched, err := s.resolveLiveConfiguredNamedMailTarget(store, recipient); err != nil {
		return "", err
	} else if matched {
		return target.display, nil
	}
	if id, err := s.resolveSessionTargetIDWithContext(ctx, store, recipient, apiSessionResolveOptions{}); err == nil {
		bead, getErr := store.Get(id)
		if getErr != nil {
			return "", getErr
		}
		address := session.MailboxAddress(bead)
		if address == "" {
			return "", fmt.Errorf("session %q has no mailbox identity", recipient)
		}
		return address, nil
	} else if !errors.Is(err, session.ErrSessionNotFound) {
		return "", err
	}
	if address, ok, err := s.configuredMailRecipientAddress(store, recipient); err != nil {
		return "", err
	} else if ok {
		return address, nil
	}
	return "", apiSessionTargetNotFound(recipient)
}

// apiMailQuery is the mailbox a mail list or count query reads: the recipient
// addresses, and — when the query named one session or one configured named
// session — the inbox that mailbox is, read in one listing
// (sessionInboxForQuery) instead of expanding each address again.
type apiMailQuery struct {
	recipients []string
	// session is the session whose own inbox the mailbox is
	// (beadmail.Provider.InboxForSession); nil when there is none.
	session *session.Info
	// routes are further mailbox routes read as they are, unexpanded, with
	// session's inbox — or alone when session is nil.
	routes []string
}

// readsInbox reports whether q names a mailbox read as one inbox.
func (q apiMailQuery) readsInbox() bool {
	return q.session != nil || len(q.routes) > 0
}

// resolveMailQueryWithContext resolves the agent a mail list or count query
// names onto the mailbox it reads.
func (s *Server) resolveMailQueryWithContext(ctx context.Context, recipient string) apiMailQuery {
	recipient = strings.TrimSpace(recipient)
	if recipient == "" {
		return apiMailQuery{recipients: []string{""}}
	}
	if recipient == "human" {
		return apiMailQuery{recipients: []string{"human"}}
	}
	store := s.state.SessionsBeadStore().Store
	if store == nil {
		if resolved, err := mail.ResolveRecipient(recipient, agentEntries(s.state.Config())); err == nil {
			if resolved == recipient {
				return apiMailQuery{recipients: []string{resolved}}
			}
			// Compatibility: older tests and direct provider callers may have
			// persisted mail under the raw bare name while API sends now
			// canonicalize to the qualified recipient.
			return apiMailQuery{recipients: []string{resolved, recipient}}
		}
		return apiMailQuery{recipients: []string{recipient}}
	}
	if spec, ok, err := s.findNamedSessionSpecForTarget(store, recipient); err == nil && ok {
		if q, listErr := s.mailQueryForNamedSession(store, spec, recipient); listErr == nil && len(q.recipients) > 0 {
			return q
		}
	}
	resolved, err := s.resolveSessionTargetIDWithContext(ctx, store, recipient, apiSessionResolveOptions{})
	if err != nil {
		return apiMailQuery{recipients: []string{recipient}}
	}
	if info, _, getErr := session.ResolveSessionRecordByExactID(store, resolved); getErr == nil {
		if recipients := session.MailboxAddressesIncludingRuntimeNameFromInfo(info); len(recipients) > 0 {
			return apiMailQuery{recipients: recipients, session: &info}
		}
	}
	return apiMailQuery{recipients: []string{resolved}}
}

// mailQueryForNamedSession is the mailbox of a configured named session that
// recipient names: the live incarnation's session inbox, plus — as routes read
// unexpanded — the configured identity, recipient itself, and every other
// incarnation's own addresses. An alias-history address is never expanded
// through the session that holds it now, so another live session's mail stays
// out, as it does for a query naming the incarnation by id. With no live
// incarnation the routes alone are the mailbox.
func (s *Server) mailQueryForNamedSession(store beads.Store, spec apiNamedSessionSpec, recipient string) (apiMailQuery, error) {
	candidates, err := s.namedSessionIncarnations(store, spec)
	if err != nil {
		return apiMailQuery{}, err
	}
	identity := apiNormalizeSessionTarget(spec.Identity)
	if identity == "" {
		return apiMailQuery{}, nil
	}
	q := apiMailQuery{recipients: []string{identity}}
	routes := []string{identity, recipient}
	for _, b := range candidates {
		addresses := session.MailboxAddressesIncludingRuntimeName(b)
		q.recipients = append(q.recipients, addresses...)
		if q.session == nil && b.Status != "closed" {
			info, _, getErr := session.ResolveSessionRecordByExactID(store, b.ID)
			if getErr != nil {
				return apiMailQuery{}, getErr
			}
			q.session = &info
			continue
		}
		routes = append(routes, addresses...)
	}
	q.recipients = uniqueNonEmptyMailRecipients(append(q.recipients, recipient))
	sort.Strings(q.recipients)
	q.routes = uniqueNonEmptyMailRecipients(routes)
	return q, nil
}

// namedSessionIncarnations returns the session beads, open and closed, of the
// configured named session spec, once each.
func (s *Server) namedSessionIncarnations(store beads.Store, spec apiNamedSessionSpec) ([]beads.Bead, error) {
	identity := apiNormalizeSessionTarget(spec.Identity)
	if identity == "" {
		return nil, nil
	}
	candidates, err := store.List(beads.ListQuery{
		Metadata: map[string]string{
			session.NamedSessionIdentityMetadata: identity,
		},
		IncludeClosed: true,
	})
	if err != nil {
		return nil, fmt.Errorf("listing named session mail recipients: %w", err)
	}
	seen := make(map[string]bool)
	out := make([]beads.Bead, 0, len(candidates))
	for _, b := range candidates {
		if !session.IsSessionBeadOrRepairable(b) ||
			!session.IsNamedSessionBead(b) ||
			session.NamedSessionIdentity(b) != spec.Identity {
			continue
		}
		if b.ID == "" || seen[b.ID] {
			continue
		}
		seen[b.ID] = true
		out = append(out, b)
	}
	return out, nil
}

func (s *Server) configuredNamedMailIdentities(identifier string) []string {
	identifier = apiNormalizeSessionTarget(identifier)
	seen := make(map[string]bool)
	identities := make([]string, 0, 2)
	add := func(identity string) {
		identity = apiNormalizeSessionTarget(identity)
		if identity == "" || seen[identity] {
			return
		}
		seen[identity] = true
		identities = append(identities, identity)
	}
	add(identifier)
	cfg := s.state.Config()
	if cfg == nil {
		return identities
	}
	for i := range cfg.NamedSessions {
		identity := cfg.NamedSessions[i].QualifiedName()
		if session.TargetBasename(identity) == identifier {
			add(identity)
		}
	}
	return identities
}

type apiResolvedMailTarget struct {
	display    string
	recipients []string
}

// WI-6 residual: this resolver stays on raw store.List + the bead-form mailbox
// accessors (MailboxAddressesIncludingRuntimeName / MailboxAddress). Its per-
// identity read is a METADATA-filtered ListQuery (configured_named_identity ==
// identity), which the session front door's ListAll(opts) does not model — ListAll
// carries only IncludeClosed/Sort/Live/Limit, not a metadata predicate. Converting
// it needs an Info-taking mailbox twin fed by a metadata-filtered store list (the
// HasOpenSessionNamed precedent), deferred to the WI-2 mail residual; until then the
// codec stays confined to the bead-form accessors here.
func (s *Server) resolveLiveConfiguredNamedMailTarget(store beads.Store, identifier string) (apiResolvedMailTarget, bool, error) {
	identifier = apiNormalizeSessionTarget(identifier)
	if store == nil || identifier == "" || identifier == "human" || strings.Contains(identifier, "/") {
		return apiResolvedMailTarget{}, false, nil
	}
	identities := s.configuredNamedMailIdentities(identifier)
	all := make([]beads.Bead, 0, len(identities))
	seenBeads := make(map[string]bool)
	for _, identity := range identities {
		items, err := store.List(beads.ListQuery{
			Metadata: map[string]string{
				session.NamedSessionIdentityMetadata: identity,
			},
		})
		if err != nil {
			return apiResolvedMailTarget{}, false, err
		}
		for _, b := range items {
			if b.ID != "" && seenBeads[b.ID] {
				continue
			}
			seenBeads[b.ID] = true
			all = append(all, b)
		}
	}

	matches := make(map[string]apiResolvedMailTarget)
	order := make([]string, 0, 2)
	for _, b := range all {
		if !session.IsSessionBeadOrRepairable(b) || b.Status == "closed" {
			continue
		}
		identity := strings.TrimSpace(b.Metadata[apiNamedSessionIdentityKey])
		if identity == "" || session.TargetBasename(identity) != identifier {
			continue
		}
		addresses := session.MailboxAddressesIncludingRuntimeName(b)
		if len(addresses) == 0 {
			continue
		}
		display := session.MailboxAddress(b)
		if display == "" {
			display = addresses[0]
		}
		if _, ok := matches[display]; ok {
			continue
		}
		matches[display] = apiResolvedMailTarget{
			display:    display,
			recipients: addresses,
		}
		order = append(order, display)
	}

	switch len(order) {
	case 0:
		return apiResolvedMailTarget{}, false, nil
	case 1:
		return matches[order[0]], true, nil
	default:
		sort.Strings(order)
		return apiResolvedMailTarget{}, false, fmt.Errorf("%w: %q matches multiple configured named session mailboxes: %s", session.ErrAmbiguous, identifier, strings.Join(order, ", "))
	}
}

func (s *Server) configuredMailRecipientAddress(store beads.Store, identifier string) (string, bool, error) {
	identifier = apiNormalizeSessionTarget(identifier)
	if identifier == "" || identifier == "human" {
		return "", false, nil
	}
	spec, ok, err := s.findNamedSessionSpecForTarget(store, identifier)
	if err != nil || !ok {
		return "", false, err
	}
	return spec.Identity, true, nil
}

// sessionInboxForQuery reads the mailbox of a query that named one session or
// one configured named session as one inbox, when mp can
// (beadmail.SessionInboxReader) — the mailbox the CLI's self-reads, hook
// injection and SessionStart mail read, so the API's answer agrees with
// theirs. ok is false for any other query or provider.
func sessionInboxForQuery(mp mail.Provider, q apiMailQuery) (inbox *beadmail.Inbox, ok bool, err error) {
	reader, reads := mp.(beadmail.SessionInboxReader)
	if !reads || !q.readsInbox() {
		return nil, false, nil
	}
	if q.session != nil {
		inbox, err = reader.InboxForSession(*q.session, q.routes...)
	} else {
		inbox, err = reader.InboxForRoutes(q.routes)
	}
	return inbox, true, err
}

// mailInboxForQuery returns the unread mail of the mailbox q names.
func mailInboxForQuery(mp mail.Provider, q apiMailQuery) ([]mail.Message, error) {
	if inbox, ok, err := sessionInboxForQuery(mp, q); ok {
		if err != nil {
			return nil, err
		}
		return inbox.Unread(), nil
	}
	recipients := uniqueMailRecipients(q.recipients)
	if inboxer, ok := mp.(mail.MultiRecipientInboxer); ok {
		return inboxer.InboxRecipients(recipients)
	}
	return mailMessagesForRecipients(mp.Inbox, recipients)
}

// mailAllForQuery returns all open mail, read or unread, of the mailbox q
// names.
func mailAllForQuery(mp mail.Provider, q apiMailQuery) ([]mail.Message, error) {
	if inbox, ok, err := sessionInboxForQuery(mp, q); ok {
		if err != nil {
			return nil, err
		}
		return inbox.All(), nil
	}
	return mailMessagesForRecipients(mp.All, q.recipients)
}

func mailMessagesForRecipients(fetch func(string) ([]mail.Message, error), recipients []string) ([]mail.Message, error) {
	recipients = uniqueMailRecipients(recipients)
	var all []mail.Message
	seen := make(map[string]bool)
	for _, recipient := range recipients {
		msgs, err := fetch(recipient)
		if err != nil {
			return nil, err
		}
		for _, msg := range msgs {
			if msg.ID != "" {
				if seen[msg.ID] {
					continue
				}
				seen[msg.ID] = true
			}
			all = append(all, msg)
		}
	}
	return all, nil
}

// mailCountForQuery returns the total and unread counts of the mailbox q
// names.
func mailCountForQuery(mp mail.Provider, q apiMailQuery) (int, int, error) {
	if inbox, ok, err := sessionInboxForQuery(mp, q); ok {
		if err != nil {
			return 0, 0, err
		}
		total, unread := inbox.Counts()
		return total, unread, nil
	}
	recipients := uniqueMailRecipients(q.recipients)
	if counter, ok := mp.(interface {
		CountRecipients([]string) (int, int, error)
	}); ok {
		return counter.CountRecipients(recipients)
	}
	var totalAll, unreadAll int
	for _, recipient := range recipients {
		total, unread, err := mp.Count(recipient)
		if err != nil {
			return 0, 0, err
		}
		totalAll += total
		unreadAll += unread
	}
	return totalAll, unreadAll, nil
}

func uniqueMailRecipients(recipients []string) []string {
	if len(recipients) == 0 {
		return []string{""}
	}
	seen := make(map[string]bool, len(recipients))
	unique := make([]string, 0, len(recipients))
	for _, recipient := range recipients {
		if seen[recipient] {
			continue
		}
		seen[recipient] = true
		unique = append(unique, recipient)
	}
	if len(unique) == 0 {
		return []string{""}
	}
	return unique
}

func uniqueNonEmptyMailRecipients(recipients []string) []string {
	seen := make(map[string]bool, len(recipients))
	unique := make([]string, 0, len(recipients))
	for _, recipient := range recipients {
		recipient = strings.TrimSpace(recipient)
		if recipient == "" || seen[recipient] {
			continue
		}
		seen[recipient] = true
		unique = append(unique, recipient)
	}
	return unique
}

// agentEntries converts city config agents to mail.AgentEntry for recipient resolution.
func agentEntries(cfg *config.City) []mail.AgentEntry {
	if cfg == nil {
		return nil
	}
	entries := make([]mail.AgentEntry, len(cfg.Agents))
	for i, a := range cfg.Agents {
		entries[i] = mail.AgentEntry{Dir: a.Dir, Name: a.Name, BindingName: a.BindingName}
	}
	return entries
}

// sortedProviderNames returns provider names in sorted order, deduplicating
// providers that share the same underlying instance (e.g. file provider mode).
func sortedProviderNames(providers map[string]mail.Provider) []string {
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	sort.Strings(names)
	seen := make(map[mail.Provider]bool, len(names))
	deduped := names[:0]
	for _, name := range names {
		p := providers[name]
		if seen[p] {
			continue
		}
		seen[p] = true
		deduped = append(deduped, name)
	}
	return deduped
}

// recordMailEvent emits a mail SSE event so SSE consumers receive
// real-time updates for API-initiated operations (not just CLI-initiated
// ones). Best-effort: silently skips if no event provider is configured.
// The input payload is typed (MailEventPayload); the json.Marshal below
// is the internal bus serialization permitted by the Principle 4 edge
// case for event-bus []byte payloads.
func (s *Server) recordMailEvent(eventType, actor, subject, rig string, msg *mail.Message) {
	ep := s.state.EventProvider()
	if ep == nil {
		return
	}
	b, _ := json.Marshal(MailEventPayload{Rig: rig, Message: msg})
	ep.Record(events.Event{
		Type:    eventType,
		Actor:   actor,
		Subject: subject,
		Payload: b,
	})
}

// tagRig stamps every message with the provider/rig name so API consumers
// can distinguish messages from different rigs in aggregated responses.
func tagRig(msgs []mail.Message, rig string) []mail.Message {
	for i := range msgs {
		msgs[i].Rig = rig
	}
	return msgs
}
