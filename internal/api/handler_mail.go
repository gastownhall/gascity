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
	store := s.state.CityBeadStore()
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
		address := apiSessionMailboxAddress(bead)
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

func (s *Server) resolveMailQueryRecipientsWithContext(ctx context.Context, recipient string) []string {
	recipient = strings.TrimSpace(recipient)
	if recipient == "" {
		return []string{""}
	}
	if recipient == "human" {
		return []string{"human"}
	}
	store := s.state.CityBeadStore()
	if store == nil {
		if resolved, err := mail.ResolveRecipient(recipient, agentEntries(s.state.Config())); err == nil {
			if resolved == recipient {
				return []string{resolved}
			}
			// Compatibility: older tests and direct provider callers may have
			// persisted mail under the raw bare name while API sends now
			// canonicalize to the qualified recipient.
			return []string{resolved, recipient}
		}
		return []string{recipient}
	}
	if spec, ok, err := s.findNamedSessionSpecForTarget(store, recipient); err == nil && ok {
		if recipients, listErr := s.mailRecipientsForNamedSession(store, spec); listErr == nil && len(recipients) > 0 {
			return append(recipients, recipient)
		}
	}
	resolved, err := s.resolveSessionTargetIDWithContext(ctx, store, recipient, apiSessionResolveOptions{})
	if err != nil {
		return []string{recipient}
	}
	return []string{resolved}
}

func (s *Server) mailRecipientsForNamedSession(store beads.Store, spec apiNamedSessionSpec) ([]string, error) {
	candidates, err := store.List(beads.ListQuery{
		Label:         session.LabelSession,
		IncludeClosed: true,
	})
	if err != nil {
		return nil, fmt.Errorf("listing named session mail recipients: %w", err)
	}
	recipients := make([]string, 0)
	seen := make(map[string]bool)
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
		recipients = append(recipients, b.ID)
	}
	sort.Strings(recipients)
	return recipients, nil
}

type apiResolvedMailTarget struct {
	display    string
	recipients []string
}

func apiSessionMailboxAddress(b beads.Bead) string {
	if alias := strings.TrimSpace(b.Metadata["alias"]); alias != "" {
		return alias
	}
	if b.ID != "" {
		return b.ID
	}
	return strings.TrimSpace(b.Metadata["session_name"])
}

func apiSessionMailboxAddresses(b beads.Bead) []string {
	seen := map[string]bool{}
	var addresses []string
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			return
		}
		seen[value] = true
		addresses = append(addresses, value)
	}
	add(apiSessionMailboxAddress(b))
	add(b.ID)
	for _, alias := range session.AliasHistory(b.Metadata) {
		add(alias)
	}
	if len(addresses) == 0 {
		add(strings.TrimSpace(b.Metadata["session_name"]))
	}
	return addresses
}

func (s *Server) resolveLiveConfiguredNamedMailTarget(store beads.Store, identifier string) (apiResolvedMailTarget, bool, error) {
	identifier = apiNormalizeSessionTarget(identifier)
	if store == nil || identifier == "" || identifier == "human" || strings.Contains(identifier, "/") {
		return apiResolvedMailTarget{}, false, nil
	}
	all, err := store.List(beads.ListQuery{
		Label: session.LabelSession,
	})
	if err != nil {
		return apiResolvedMailTarget{}, false, err
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
		addresses := apiSessionMailboxAddresses(b)
		if len(addresses) == 0 {
			continue
		}
		display := apiSessionMailboxAddress(b)
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

func mailInboxForRecipients(mp mail.Provider, recipients []string) ([]mail.Message, error) {
	return mailMessagesForRecipients(mp.Inbox, recipients)
}

func mailAllForRecipients(mp mail.Provider, recipients []string) ([]mail.Message, error) {
	return mailMessagesForRecipients(mp.All, recipients)
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

func mailCountForRecipients(mp mail.Provider, recipients []string) (int, int, error) {
	recipients = uniqueMailRecipients(recipients)
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
	unique := recipients[:0]
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
