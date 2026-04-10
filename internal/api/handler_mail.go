package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail"
)

func (s *Server) handleMailList(w http.ResponseWriter, r *http.Request) {
	bp := parseBlockingParams(r)
	if bp.isBlocking() {
		waitForChange(r.Context(), s.state.EventProvider(), bp)
	}

	q := r.URL.Query()
	agent := q.Get("agent")
	status := q.Get("status")
	rig := q.Get("rig")

	switch status {
	case "", "unread":
		pp := parsePagination(r, 50)

		// Aggregate across all rigs when rig is omitted (matching count semantics).
		if rig != "" {
			mp := s.state.MailProvider(rig)
			if mp == nil {
				writeListJSON(w, s.latestIndex(), []any{}, 0)
				return
			}
			msgs, err := mp.Inbox(agent)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "internal", err.Error())
				return
			}
			if msgs == nil {
				msgs = []mail.Message{}
			}
			msgs = tagRig(msgs, rig)
			if !pp.IsPaging {
				total := len(msgs)
				if pp.Limit < len(msgs) {
					msgs = msgs[:pp.Limit]
				}
				writeListJSON(w, s.latestIndex(), msgs, total)
				return
			}
			page, total, nextCursor := paginate(msgs, pp)
			if page == nil {
				page = []mail.Message{}
			}
			writePagedJSON(w, s.latestIndex(), page, total, nextCursor)
			return
		}

		providers := s.state.MailProviders()
		var allMsgs []mail.Message
		for _, name := range sortedProviderNames(providers) {
			msgs, err := providers[name].Inbox(agent)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "internal", "mail provider "+name+": "+err.Error())
				return
			}
			allMsgs = append(allMsgs, tagRig(msgs, name)...)
		}
		if allMsgs == nil {
			allMsgs = []mail.Message{}
		}
		if !pp.IsPaging {
			total := len(allMsgs)
			if pp.Limit < len(allMsgs) {
				allMsgs = allMsgs[:pp.Limit]
			}
			writeListJSON(w, s.latestIndex(), allMsgs, total)
			break
		}
		page, total, nextCursor := paginate(allMsgs, pp)
		if page == nil {
			page = []mail.Message{}
		}
		writePagedJSON(w, s.latestIndex(), page, total, nextCursor)
	case "all":
		pp := parsePagination(r, 50)

		if rig != "" {
			mp := s.state.MailProvider(rig)
			if mp == nil {
				writeListJSON(w, s.latestIndex(), []any{}, 0)
				return
			}
			msgs, err := mp.All(agent)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "internal", err.Error())
				return
			}
			if msgs == nil {
				msgs = []mail.Message{}
			}
			msgs = tagRig(msgs, rig)
			if !pp.IsPaging {
				total := len(msgs)
				if pp.Limit < len(msgs) {
					msgs = msgs[:pp.Limit]
				}
				writeListJSON(w, s.latestIndex(), msgs, total)
				return
			}
			page, total, nextCursor := paginate(msgs, pp)
			if page == nil {
				page = []mail.Message{}
			}
			writePagedJSON(w, s.latestIndex(), page, total, nextCursor)
			return
		}

		providers := s.state.MailProviders()
		var allMsgs []mail.Message
		for _, name := range sortedProviderNames(providers) {
			msgs, err := providers[name].All(agent)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "internal", "mail provider "+name+": "+err.Error())
				return
			}
			allMsgs = append(allMsgs, tagRig(msgs, name)...)
		}
		if allMsgs == nil {
			allMsgs = []mail.Message{}
		}
		if !pp.IsPaging {
			total := len(allMsgs)
			if pp.Limit < len(allMsgs) {
				allMsgs = allMsgs[:pp.Limit]
			}
			writeListJSON(w, s.latestIndex(), allMsgs, total)
			break
		}
		page, total, nextCursor := paginate(allMsgs, pp)
		if page == nil {
			page = []mail.Message{}
		}
		writePagedJSON(w, s.latestIndex(), page, total, nextCursor)
	default:
		writeError(w, http.StatusBadRequest, "invalid", "unsupported status filter: "+status+"; supported: unread, all")
	}
}

func (s *Server) handleMailGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rig := r.URL.Query().Get("rig")
	mp, resolvedRig, err := s.findMailProviderForMessage(id, rig)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if mp == nil {
		writeError(w, http.StatusNotFound, "not_found", "message "+id+" not found")
		return
	}

	msg, err := mp.Get(id)
	if err != nil {
		if errors.Is(err, mail.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
		}
		return
	}
	msg.Rig = resolvedRig
	writeIndexJSON(w, s.latestIndex(), msg)
}

func (s *Server) handleMailSend(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Rig     string `json:"rig"`
		From    string `json:"from"`
		To      string `json:"to"`
		Subject string `json:"subject"`
		Body    string `json:"body"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}

	var errs []FieldError
	if body.To == "" {
		errs = append(errs, FieldError{Field: "to", Message: "required"})
	}
	if body.Subject == "" {
		errs = append(errs, FieldError{Field: "subject", Message: "required"})
	}
	if len(errs) > 0 {
		writeJSON(w, http.StatusBadRequest, Error{
			Code:    "invalid",
			Message: "invalid mail request",
			Details: errs,
		})
		return
	}

	// Resolve recipient against configured agents.
	resolved, resolveErr := mail.ResolveRecipient(body.To, agentEntries(s.state.Config()))
	if resolveErr != nil {
		writeError(w, http.StatusBadRequest, "invalid", resolveErr.Error())
		return
	}

	mp := s.findMailProvider(body.Rig)
	if mp == nil {
		writeError(w, http.StatusBadRequest, "invalid", "no mail provider available")
		return
	}

	// Idempotency check — key is scoped by method+path to prevent cross-endpoint collisions.
	idemKey := scopedIdemKey(r, r.Header.Get("Idempotency-Key"))
	var bodyHash string
	if idemKey != "" {
		bodyHash = hashBody(body)
		if s.idem.handleIdempotent(w, idemKey, bodyHash) {
			return
		}
	}

	msg, err := mp.Send(body.From, resolved, body.Subject, body.Body)
	if err != nil {
		s.idem.unreserve(idemKey)
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	msg.Rig = body.Rig
	s.idem.storeResponse(idemKey, bodyHash, http.StatusCreated, msg)
	s.recordMailEvent(events.MailSent, body.From, msg.ID, body.Rig, &msg)
	writeJSON(w, http.StatusCreated, msg)
}

func (s *Server) handleMailRead(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rig := r.URL.Query().Get("rig")
	mp, resolvedRig, err := s.findMailProviderForMessage(id, rig)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if mp == nil {
		writeError(w, http.StatusNotFound, "not_found", "message "+id+" not found")
		return
	}
	if err := mp.MarkRead(id); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.recordMailEvent(events.MailMarkedRead, "api", id, resolvedRig, nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "read"})
}

func (s *Server) handleMailMarkUnread(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rig := r.URL.Query().Get("rig")
	mp, resolvedRig, err := s.findMailProviderForMessage(id, rig)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if mp == nil {
		writeError(w, http.StatusNotFound, "not_found", "message "+id+" not found")
		return
	}
	if err := mp.MarkUnread(id); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.recordMailEvent(events.MailMarkedUnread, "api", id, resolvedRig, nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "unread"})
}

func (s *Server) handleMailArchive(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rig := r.URL.Query().Get("rig")
	mp, resolvedRig, err := s.findMailProviderForMessage(id, rig)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if mp == nil {
		writeError(w, http.StatusNotFound, "not_found", "message "+id+" not found")
		return
	}
	if err := mp.Archive(id); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.recordMailEvent(events.MailArchived, "api", id, resolvedRig, nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "archived"})
}

func (s *Server) handleMailReply(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rig := r.URL.Query().Get("rig")
	var body struct {
		From    string `json:"from"`
		Subject string `json:"subject"`
		Body    string `json:"body"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}

	mp, resolvedRig, mpErr := s.findMailProviderForMessage(id, rig)
	if mpErr != nil {
		writeError(w, http.StatusInternalServerError, "internal", mpErr.Error())
		return
	}
	if mp == nil {
		writeError(w, http.StatusNotFound, "not_found", "message "+id+" not found")
		return
	}

	msg, err := mp.Reply(id, body.From, body.Subject, body.Body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	msg.Rig = resolvedRig
	s.recordMailEvent(events.MailReplied, body.From, msg.ID, resolvedRig, &msg)
	writeJSON(w, http.StatusCreated, msg)
}

func (s *Server) handleMailDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rig := r.URL.Query().Get("rig")
	mp, resolvedRig, err := s.findMailProviderForMessage(id, rig)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if mp == nil {
		writeError(w, http.StatusNotFound, "not_found", "message "+id+" not found")
		return
	}
	if err := mp.Delete(id); err != nil {
		if errors.Is(err, mail.ErrNotFound) || errors.Is(err, beads.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "message "+id+" not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.recordMailEvent(events.MailDeleted, "api", id, resolvedRig, nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleMailThread(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("id")
	rig := r.URL.Query().Get("rig")

	// When rig is specified, query only that provider.
	if rig != "" {
		mp := s.state.MailProvider(rig)
		if mp == nil {
			writeError(w, http.StatusNotFound, "not_found", "rig "+rig+" not found")
			return
		}
		msgs, err := mp.Thread(threadID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		if msgs == nil {
			msgs = []mail.Message{}
		}
		msgs = tagRig(msgs, rig)
		writeListJSON(w, s.latestIndex(), msgs, len(msgs))
		return
	}

	// Aggregate thread messages across all providers.
	providers := s.state.MailProviders()
	var allMsgs []mail.Message
	for _, name := range sortedProviderNames(providers) {
		msgs, err := providers[name].Thread(threadID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "mail provider "+name+": "+err.Error())
			return
		}
		allMsgs = append(allMsgs, tagRig(msgs, name)...)
	}
	if allMsgs == nil {
		allMsgs = []mail.Message{}
	}
	writeListJSON(w, s.latestIndex(), allMsgs, len(allMsgs))
}

func (s *Server) handleMailCount(w http.ResponseWriter, r *http.Request) {
	agentName := r.URL.Query().Get("agent")
	rig := r.URL.Query().Get("rig")

	// If rig specified, count only that rig.
	if rig != "" {
		mp := s.state.MailProvider(rig)
		if mp == nil {
			writeJSON(w, http.StatusOK, map[string]int{"total": 0, "unread": 0})
			return
		}
		total, unread, err := mp.Count(agentName)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]int{"total": total, "unread": unread})
		return
	}

	// Aggregate across all rigs (deduplicated by provider identity).
	providers := s.state.MailProviders()
	var totalAll, unreadAll int
	for _, name := range sortedProviderNames(providers) {
		total, unread, err := providers[name].Count(agentName)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "mail provider "+name+": "+err.Error())
			return
		}
		totalAll += total
		unreadAll += unread
	}
	writeJSON(w, http.StatusOK, map[string]int{"total": totalAll, "unread": unreadAll})
}

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

// recordMailEvent emits a mail SSE event so WebSocket/SSE consumers receive
// real-time updates for API-initiated operations (not just CLI-initiated ones).
// Best-effort: silently skips if no event provider is configured.
func (s *Server) recordMailEvent(eventType, actor, subject, rig string, msg *mail.Message) {
	ep := s.state.EventProvider()
	if ep == nil {
		return
	}
	payload := map[string]any{"rig": rig}
	if msg != nil {
		payload["message"] = msg
	}
	b, _ := json.Marshal(payload)
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
