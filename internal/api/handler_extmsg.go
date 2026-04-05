package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/session"
)

// --- helpers ---

// extmsgServices returns the extmsg services from state, writing an error
// response and returning nil if unavailable.
func (s *Server) extmsgServices(w http.ResponseWriter) *extmsg.Services {
	svc := s.state.ExtMsgServices()
	if svc == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "external messaging not enabled")
		return nil
	}
	return svc
}

// extmsgAdapterRegistry returns the adapter registry from state, writing an
// error response and returning nil if unavailable.
func (s *Server) extmsgAdapterRegistry(w http.ResponseWriter) *extmsg.AdapterRegistry {
	reg := s.state.AdapterRegistry()
	if reg == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "adapter registry not available")
		return nil
	}
	return reg
}

// extmsgEmitEvent builds an event emitter closure for extmsg handlers.
func (s *Server) extmsgEmitEvent() func(string, string, map[string]any) {
	ep := s.state.EventProvider()
	if ep == nil {
		return func(string, string, map[string]any) {}
	}
	return func(eventType, subject string, payload map[string]any) {
		b, err := json.Marshal(payload)
		if err != nil {
			fmt.Fprintf(os.Stderr, "extmsg: marshal event payload: %v\n", err)
			return
		}
		ep.Record(events.Event{
			Type:    eventType,
			Subject: subject,
			Payload: b,
		})
	}
}

// extmsgNotifyMembers sends a "check transcript" message to all transcript
// members via the session message API. This ensures delivery regardless of
// session state: sleeping sessions are woken, idle sessions get a new prompt
// turn that triggers the transcript check hook.
func (s *Server) extmsgNotifyMembers(conv extmsg.ConversationRef, inboundMsg extmsg.ExternalInboundMessage) {
	svc := s.state.ExtMsgServices()
	store := s.state.CityBeadStore()
	if svc == nil || store == nil {
		return
	}
	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "extmsg-notify"}
	members, err := svc.Transcript.ListMemberships(context.Background(), caller, conv)
	if err != nil {
		log.Printf("extmsg: ListMemberships failed for %s/%s: %v", conv.Provider, conv.ConversationID, err)
		return
	}

	actorKind := "agent"
	if !inboundMsg.Actor.IsBot {
		actorKind = "human"
	}

	ctx := context.Background()
	var wg sync.WaitGroup
	for _, m := range members {
		wg.Add(1)
		go func(sessionID string) {
			defer wg.Done()
			// Resolve the member's handle from their session bead alias.
			// Membership stores session names (s-et-xxxx); bead IDs drop the "s-" prefix.
			handle := sessionID
			beadID := strings.TrimPrefix(sessionID, "s-")
			if b, err := store.Get(beadID); err == nil {
				if alias := b.Metadata["alias"]; alias != "" {
					if idx := strings.LastIndex(alias, "/"); idx >= 0 {
						handle = alias[idx+1:]
					} else {
						handle = alias
					}
				}
			}
			nudge := fmt.Sprintf("<system-reminder>\nNew message in shared conversation %s/%s:\n\n"+
				"- %s (%s): %s\n\n"+
				"To reply in Discord, write your response to a file and run:\n"+
				"  gc discord reply-current --conversation-id %s --body-file <path>\n"+
				"Prefix your reply with your agent handle in bold (e.g., **%s:** your message).\n"+
				"Run 'gc transcript read --ack' after responding to mark as read.\n"+
				"</system-reminder>",
				conv.Provider, conv.ConversationID,
				inboundMsg.Actor.DisplayName, actorKind, inboundMsg.Text,
				conv.ConversationID,
				handle,
			)
			// Resolve session identifier to bead ID, then send.
			resolvedID, err := session.ResolveSessionID(store, sessionID)
			if err != nil {
				log.Printf("extmsg: resolve session %s failed: %v", sessionID, err)
				return
			}
			if err := s.sendMessageToSession(ctx, store, resolvedID, nudge); err != nil {
				log.Printf("extmsg: notify %s failed: %v", sessionID, err)
			}
		}(m.SessionID)
	}
	wg.Wait()
}

// --- inbound ---

func (s *Server) handleExtMsgInbound(w http.ResponseWriter, r *http.Request) {
	svc := s.extmsgServices(w)
	if svc == nil {
		return
	}
	reg := s.extmsgAdapterRegistry(w)
	if reg == nil {
		return
	}

	var body struct {
		// For pre-normalized messages (out-of-process adapters):
		Message *extmsg.ExternalInboundMessage `json:"message,omitempty"`
		// For raw payloads (in-process adapters):
		Provider  string `json:"provider,omitempty"`
		AccountID string `json:"account_id,omitempty"`
		Payload   []byte `json:"payload,omitempty"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}

	deps := extmsg.InboundDeps{
		Services:  *svc,
		Registry:  reg,
		EmitEvent: s.extmsgEmitEvent(),
	}

	ctx := r.Context()

	// Pre-normalized path.
	if body.Message != nil {
		result, err := extmsg.HandleInboundNormalized(ctx, deps, *body.Message)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, "processing_failed", err.Error())
			return
		}
		go s.extmsgNotifyMembers(body.Message.Conversation, *body.Message)
		writeJSON(w, http.StatusOK, result)
		return
	}

	// Raw payload path.
	if body.Provider == "" || body.AccountID == "" {
		writeError(w, http.StatusBadRequest, "invalid", "provider and account_id are required for raw payloads")
		return
	}

	key := extmsg.AdapterKey{Provider: body.Provider, AccountID: body.AccountID}
	result, err := extmsg.HandleInbound(ctx, deps, key, extmsg.InboundPayload{
		Body:       body.Payload,
		ReceivedAt: time.Now(),
	})
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "processing_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// --- outbound ---

func (s *Server) handleExtMsgOutbound(w http.ResponseWriter, r *http.Request) {
	svc := s.extmsgServices(w)
	if svc == nil {
		return
	}
	reg := s.extmsgAdapterRegistry(w)
	if reg == nil {
		return
	}

	var body struct {
		SessionID        string                 `json:"session_id"`
		Conversation     extmsg.ConversationRef `json:"conversation"`
		Text             string                 `json:"text"`
		ReplyToMessageID string                 `json:"reply_to_message_id,omitempty"`
		IdempotencyKey   string                 `json:"idempotency_key,omitempty"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	if body.SessionID == "" {
		writeError(w, http.StatusBadRequest, "invalid", "session_id is required")
		return
	}

	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "api"}
	deps := extmsg.OutboundDeps{
		Services:  *svc,
		Registry:  reg,
		EmitEvent: s.extmsgEmitEvent(),
	}

	result, err := extmsg.HandleOutbound(r.Context(), deps, caller, extmsg.OutboundRequest{
		SessionID:        body.SessionID,
		Conversation:     body.Conversation,
		Text:             body.Text,
		ReplyToMessageID: body.ReplyToMessageID,
		IdempotencyKey:   body.IdempotencyKey,
	})
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "processing_failed", err.Error())
		return
	}
	go s.extmsgNotifyMembers(body.Conversation, extmsg.ExternalInboundMessage{
		Conversation: body.Conversation,
		Actor:        extmsg.ExternalActor{ID: body.SessionID, DisplayName: body.SessionID, IsBot: true},
		Text:         body.Text,
	})
	writeJSON(w, http.StatusOK, result)
}

// --- bindings ---

func (s *Server) handleExtMsgBindingList(w http.ResponseWriter, r *http.Request) {
	svc := s.extmsgServices(w)
	if svc == nil {
		return
	}

	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "invalid", "session_id query parameter is required")
		return
	}

	bindings, err := svc.Bindings.ListBySession(r.Context(), sessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if bindings == nil {
		bindings = []extmsg.SessionBindingRecord{}
	}
	writeListJSON(w, s.latestIndex(), bindings, len(bindings))
}

func (s *Server) handleExtMsgBind(w http.ResponseWriter, r *http.Request) {
	svc := s.extmsgServices(w)
	if svc == nil {
		return
	}

	var body struct {
		Conversation extmsg.ConversationRef `json:"conversation"`
		SessionID    string                 `json:"session_id"`
		Metadata     map[string]string      `json:"metadata,omitempty"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	if body.SessionID == "" {
		writeError(w, http.StatusBadRequest, "invalid", "session_id is required")
		return
	}

	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "api"}
	binding, err := svc.Bindings.Bind(r.Context(), caller, extmsg.BindInput{
		Conversation: body.Conversation,
		SessionID:    body.SessionID,
		Metadata:     body.Metadata,
		Now:          time.Now(),
	})
	if err != nil {
		switch {
		case errors.Is(err, extmsg.ErrBindingConflict):
			writeError(w, http.StatusConflict, "conflict", err.Error())
		case errors.Is(err, extmsg.ErrInvalidInput) || errors.Is(err, extmsg.ErrInvalidConversation):
			writeError(w, http.StatusBadRequest, "invalid", err.Error())
		default:
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
		}
		return
	}

	s.extmsgEmitEvent()(events.ExtMsgBound, body.SessionID, map[string]any{
		"provider":        body.Conversation.Provider,
		"conversation_id": body.Conversation.ConversationID,
		"session_id":      body.SessionID,
	})
	writeJSON(w, http.StatusCreated, binding)
}

func (s *Server) handleExtMsgUnbind(w http.ResponseWriter, r *http.Request) {
	svc := s.extmsgServices(w)
	if svc == nil {
		return
	}

	var body struct {
		Conversation *extmsg.ConversationRef `json:"conversation,omitempty"`
		SessionID    string                  `json:"session_id"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	if body.SessionID == "" {
		writeError(w, http.StatusBadRequest, "invalid", "session_id is required")
		return
	}

	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "api"}
	unbound, err := svc.Bindings.Unbind(r.Context(), caller, extmsg.UnbindInput{
		Conversation: body.Conversation,
		SessionID:    body.SessionID,
		Now:          time.Now(),
	})
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "processing_failed", err.Error())
		return
	}

	s.extmsgEmitEvent()(events.ExtMsgUnbound, body.SessionID, map[string]any{
		"session_id": body.SessionID,
		"count":      len(unbound),
	})
	writeJSON(w, http.StatusOK, map[string]any{"unbound": unbound})
}

// --- groups ---

func (s *Server) handleExtMsgGroupLookup(w http.ResponseWriter, r *http.Request) {
	// Read-only lookup of a group by root conversation query params.
	// Does NOT create a group — use POST /v0/extmsg/groups for that.
	svc := s.extmsgServices(w)
	if svc == nil {
		return
	}

	q := r.URL.Query()
	ref := extmsg.ConversationRef{
		ScopeID:        q.Get("scope_id"),
		Provider:       q.Get("provider"),
		AccountID:      q.Get("account_id"),
		ConversationID: q.Get("conversation_id"),
		Kind:           extmsg.ConversationKind(q.Get("kind")),
	}

	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "api"}
	group, err := svc.Groups.FindByConversation(r.Context(), caller, ref)
	if err != nil {
		if errors.Is(err, extmsg.ErrGroupNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "group not found for conversation")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, group)
}

func (s *Server) handleExtMsgGroupEnsure(w http.ResponseWriter, r *http.Request) {
	svc := s.extmsgServices(w)
	if svc == nil {
		return
	}

	var body struct {
		RootConversation extmsg.ConversationRef `json:"root_conversation"`
		Mode             extmsg.GroupMode       `json:"mode"`
		DefaultHandle    string                 `json:"default_handle,omitempty"`
		Metadata         map[string]string      `json:"metadata,omitempty"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	if body.Mode == "" {
		body.Mode = extmsg.GroupModeLauncher
	}

	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "api"}
	group, err := svc.Groups.EnsureGroup(r.Context(), caller, extmsg.EnsureGroupInput{
		RootConversation: body.RootConversation,
		Mode:             body.Mode,
		DefaultHandle:    body.DefaultHandle,
		Metadata:         body.Metadata,
	})
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "processing_failed", err.Error())
		return
	}

	s.extmsgEmitEvent()(events.ExtMsgGroupCreated, group.ID, map[string]any{
		"provider":        body.RootConversation.Provider,
		"conversation_id": body.RootConversation.ConversationID,
		"mode":            string(body.Mode),
	})
	writeJSON(w, http.StatusCreated, group)
}

func (s *Server) handleExtMsgParticipantUpsert(w http.ResponseWriter, r *http.Request) {
	svc := s.extmsgServices(w)
	if svc == nil {
		return
	}

	var body struct {
		GroupID   string            `json:"group_id"`
		Handle    string            `json:"handle"`
		SessionID string            `json:"session_id"`
		Public    bool              `json:"public"`
		Metadata  map[string]string `json:"metadata,omitempty"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	if body.GroupID == "" || body.Handle == "" || body.SessionID == "" {
		writeError(w, http.StatusBadRequest, "invalid", "group_id, handle, and session_id are required")
		return
	}

	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "api"}
	participant, err := svc.Groups.UpsertParticipant(r.Context(), caller, extmsg.UpsertParticipantInput{
		GroupID:   body.GroupID,
		Handle:    body.Handle,
		SessionID: body.SessionID,
		Public:    body.Public,
		Metadata:  body.Metadata,
	})
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "processing_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, participant)
}

func (s *Server) handleExtMsgParticipantRemove(w http.ResponseWriter, r *http.Request) {
	svc := s.extmsgServices(w)
	if svc == nil {
		return
	}

	var body struct {
		GroupID string `json:"group_id"`
		Handle  string `json:"handle"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	if body.GroupID == "" || body.Handle == "" {
		writeError(w, http.StatusBadRequest, "invalid", "group_id and handle are required")
		return
	}

	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "api"}
	err := svc.Groups.RemoveParticipant(r.Context(), caller, extmsg.RemoveParticipantInput{
		GroupID: body.GroupID,
		Handle:  body.Handle,
	})
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "processing_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

// --- transcript ---

func (s *Server) handleExtMsgTranscriptList(w http.ResponseWriter, r *http.Request) {
	svc := s.extmsgServices(w)
	if svc == nil {
		return
	}

	q := r.URL.Query()
	ref := extmsg.ConversationRef{
		ScopeID:              q.Get("scope_id"),
		Provider:             q.Get("provider"),
		AccountID:            q.Get("account_id"),
		ConversationID:       q.Get("conversation_id"),
		ParentConversationID: q.Get("parent_conversation_id"),
		Kind:                 extmsg.ConversationKind(q.Get("kind")),
	}

	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "api"}
	entries, err := svc.Transcript.List(r.Context(), extmsg.ListTranscriptInput{
		Caller:       caller,
		Conversation: ref,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if entries == nil {
		entries = []extmsg.ConversationTranscriptRecord{}
	}
	writeListJSON(w, s.latestIndex(), entries, len(entries))
}

func (s *Server) handleExtMsgTranscriptAck(w http.ResponseWriter, r *http.Request) {
	svc := s.extmsgServices(w)
	if svc == nil {
		return
	}

	var body struct {
		Conversation extmsg.ConversationRef `json:"conversation"`
		SessionID    string                 `json:"session_id"`
		Sequence     int64                  `json:"sequence"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	if body.SessionID == "" {
		writeError(w, http.StatusBadRequest, "invalid", "session_id is required")
		return
	}

	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "api"}
	err := svc.Transcript.Ack(r.Context(), extmsg.AckMembershipInput{
		Caller:       caller,
		Conversation: body.Conversation,
		SessionID:    body.SessionID,
		Sequence:     body.Sequence,
	})
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "processing_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "acked"})
}

// --- adapters ---

func (s *Server) handleExtMsgAdapterList(w http.ResponseWriter, _ *http.Request) {
	reg := s.extmsgAdapterRegistry(w)
	if reg == nil {
		return
	}

	keys := reg.List()
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Provider != keys[j].Provider {
			return keys[i].Provider < keys[j].Provider
		}
		return keys[i].AccountID < keys[j].AccountID
	})
	type adapterInfo struct {
		Provider  string `json:"provider"`
		AccountID string `json:"account_id"`
		Name      string `json:"name"`
	}
	items := make([]adapterInfo, 0, len(keys))
	for _, k := range keys {
		a := reg.Lookup(k)
		name := ""
		if a != nil {
			name = a.Name()
		}
		items = append(items, adapterInfo{
			Provider:  k.Provider,
			AccountID: k.AccountID,
			Name:      name,
		})
	}
	writeListJSON(w, s.latestIndex(), items, len(items))
}

func (s *Server) handleExtMsgAdapterRegister(w http.ResponseWriter, r *http.Request) {
	reg := s.extmsgAdapterRegistry(w)
	if reg == nil {
		return
	}

	var body struct {
		Provider     string                     `json:"provider"`
		AccountID    string                     `json:"account_id"`
		Name         string                     `json:"name"`
		CallbackURL  string                     `json:"callback_url"`
		Capabilities extmsg.AdapterCapabilities `json:"capabilities"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	if body.Provider == "" || body.AccountID == "" {
		writeError(w, http.StatusBadRequest, "invalid", "provider and account_id are required")
		return
	}
	if body.Name == "" {
		body.Name = body.Provider + "/" + body.AccountID
	}

	adapter := extmsg.NewHTTPAdapter(body.Name, body.CallbackURL, body.Capabilities)
	key := extmsg.AdapterKey{Provider: body.Provider, AccountID: body.AccountID}
	reg.Register(key, adapter)

	s.extmsgEmitEvent()(events.ExtMsgAdapterAdded, body.Name, map[string]any{
		"provider":   body.Provider,
		"account_id": body.AccountID,
	})
	writeJSON(w, http.StatusCreated, map[string]string{
		"status":     "registered",
		"provider":   body.Provider,
		"account_id": body.AccountID,
		"name":       body.Name,
	})
}

func (s *Server) handleExtMsgAdapterUnregister(w http.ResponseWriter, r *http.Request) {
	reg := s.extmsgAdapterRegistry(w)
	if reg == nil {
		return
	}

	var body struct {
		Provider  string `json:"provider"`
		AccountID string `json:"account_id"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	if body.Provider == "" || body.AccountID == "" {
		writeError(w, http.StatusBadRequest, "invalid", "provider and account_id are required")
		return
	}

	key := extmsg.AdapterKey{Provider: body.Provider, AccountID: body.AccountID}
	reg.Unregister(key)

	s.extmsgEmitEvent()(events.ExtMsgAdapterRemoved, body.Provider+"/"+body.AccountID, map[string]any{
		"provider":   body.Provider,
		"account_id": body.AccountID,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "unregistered"})
}
