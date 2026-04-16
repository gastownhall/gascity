package api

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/extmsg"
)

// --- Huma helpers for extmsg ---

// humaExtmsgServices returns the extmsg services from state, returning an error
// if unavailable.
func (s *Server) humaExtmsgServices() (*extmsg.Services, error) {
	svc := s.state.ExtMsgServices()
	if svc == nil {
		return nil, huma.Error503ServiceUnavailable("external messaging not enabled")
	}
	return svc, nil
}

// humaExtmsgAdapterRegistry returns the adapter registry from state, returning
// an error if unavailable.
func (s *Server) humaExtmsgAdapterRegistry() (*extmsg.AdapterRegistry, error) {
	reg := s.state.AdapterRegistry()
	if reg == nil {
		return nil, huma.Error503ServiceUnavailable("adapter registry not available")
	}
	return reg, nil
}

// --- Inbound ---

// humaHandleExtMsgInbound is the Huma-typed handler for POST /v0/extmsg/inbound.
func (s *Server) humaHandleExtMsgInbound(ctx context.Context, input *ExtMsgInboundInput) (*ExtMsgInboundOutput, error) {
	svc, err := s.humaExtmsgServices()
	if err != nil {
		return nil, err
	}
	reg, err := s.humaExtmsgAdapterRegistry()
	if err != nil {
		return nil, err
	}

	deps := extmsg.InboundDeps{
		Services:  *svc,
		Registry:  reg,
		EmitEvent: s.extmsgEmitEvent(),
	}

	// Pre-normalized path.
	if input.Body.Message != nil {
		result, handleErr := extmsg.HandleInboundNormalized(ctx, deps, *input.Body.Message)
		if handleErr != nil {
			return nil, huma.Error422UnprocessableEntity(handleErr.Error())
		}
		go s.extmsgNotifyMembers(input.Body.Message.Conversation, *input.Body.Message)
		raw, _ := json.Marshal(result)
		out := &ExtMsgInboundOutput{}
		out.Body = raw
		return out, nil
	}

	// Raw payload path.
	if input.Body.Provider == "" || input.Body.AccountID == "" {
		return nil, huma.Error400BadRequest("provider and account_id are required for raw payloads")
	}

	key := extmsg.AdapterKey{Provider: input.Body.Provider, AccountID: input.Body.AccountID}
	result, err := extmsg.HandleInbound(ctx, deps, key, extmsg.InboundPayload{
		Body:       input.Body.Payload,
		ReceivedAt: time.Now(),
	})
	if err != nil {
		return nil, huma.Error422UnprocessableEntity(err.Error())
	}
	raw, _ := json.Marshal(result)
	out := &ExtMsgInboundOutput{}
	out.Body = raw
	return out, nil
}

// --- Outbound ---

// humaHandleExtMsgOutbound is the Huma-typed handler for POST /v0/extmsg/outbound.
func (s *Server) humaHandleExtMsgOutbound(ctx context.Context, input *ExtMsgOutboundInput) (*ExtMsgOutboundOutput, error) {
	svc, err := s.humaExtmsgServices()
	if err != nil {
		return nil, err
	}
	reg, err := s.humaExtmsgAdapterRegistry()
	if err != nil {
		return nil, err
	}

	if input.Body.SessionID == "" {
		return nil, huma.Error400BadRequest("session_id is required")
	}

	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "api"}
	deps := extmsg.OutboundDeps{
		Services:  *svc,
		Registry:  reg,
		EmitEvent: s.extmsgEmitEvent(),
	}

	result, err := extmsg.HandleOutbound(ctx, deps, caller, extmsg.OutboundRequest{
		SessionID:        input.Body.SessionID,
		Conversation:     input.Body.Conversation,
		Text:             input.Body.Text,
		ReplyToMessageID: input.Body.ReplyToMessageID,
		IdempotencyKey:   input.Body.IdempotencyKey,
	})
	if err != nil {
		return nil, huma.Error422UnprocessableEntity(err.Error())
	}
	go s.extmsgNotifyMembers(input.Body.Conversation, extmsg.ExternalInboundMessage{
		Conversation: input.Body.Conversation,
		Actor:        extmsg.ExternalActor{ID: input.Body.SessionID, DisplayName: input.Body.SessionID, IsBot: true},
		Text:         input.Body.Text,
	})
	raw, _ := json.Marshal(result)
	out := &ExtMsgOutboundOutput{}
	out.Body = raw
	return out, nil
}

// --- Bindings ---

// humaHandleExtMsgBindingList is the Huma-typed handler for GET /v0/extmsg/bindings.
func (s *Server) humaHandleExtMsgBindingList(ctx context.Context, input *ExtMsgBindingListInput) (*ListOutput[json.RawMessage], error) {
	svc, err := s.humaExtmsgServices()
	if err != nil {
		return nil, err
	}

	if input.SessionID == "" {
		return nil, huma.Error400BadRequest("session_id query parameter is required")
	}

	bindings, err := svc.Bindings.ListBySession(ctx, input.SessionID)
	if err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}
	if bindings == nil {
		bindings = []extmsg.SessionBindingRecord{}
	}
	rawItems := make([]json.RawMessage, len(bindings))
	for i, b := range bindings {
		raw, _ := json.Marshal(b)
		rawItems[i] = raw
	}
	return &ListOutput[json.RawMessage]{
		Index: s.latestIndex(),
		Body:  ListBody[json.RawMessage]{Items: rawItems, Total: len(rawItems)},
	}, nil
}

// humaHandleExtMsgBind is the Huma-typed handler for POST /v0/extmsg/bind.
func (s *Server) humaHandleExtMsgBind(ctx context.Context, input *ExtMsgBindInput) (*ExtMsgBindOutput, error) {
	svc, err := s.humaExtmsgServices()
	if err != nil {
		return nil, err
	}

	if input.Body.SessionID == "" {
		return nil, huma.Error400BadRequest("session_id is required")
	}

	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "api"}
	binding, err := svc.Bindings.Bind(ctx, caller, extmsg.BindInput{
		Conversation: input.Body.Conversation,
		SessionID:    input.Body.SessionID,
		Metadata:     input.Body.Metadata,
		Now:          time.Now(),
	})
	if err != nil {
		switch {
		case errors.Is(err, extmsg.ErrBindingConflict):
			return nil, huma.Error409Conflict(err.Error())
		case errors.Is(err, extmsg.ErrInvalidInput) || errors.Is(err, extmsg.ErrInvalidConversation):
			return nil, huma.Error400BadRequest(err.Error())
		default:
			return nil, huma.Error500InternalServerError(err.Error())
		}
	}

	s.extmsgEmitEvent()(events.ExtMsgBound, input.Body.SessionID, map[string]any{
		"provider":        input.Body.Conversation.Provider,
		"conversation_id": input.Body.Conversation.ConversationID,
		"session_id":      input.Body.SessionID,
	})
	raw, _ := json.Marshal(binding)
	out := &ExtMsgBindOutput{}
	out.Body = raw
	return out, nil
}

// humaHandleExtMsgUnbind is the Huma-typed handler for POST /v0/extmsg/unbind.
func (s *Server) humaHandleExtMsgUnbind(ctx context.Context, input *ExtMsgUnbindInput) (*ExtMsgUnbindOutput, error) {
	svc, err := s.humaExtmsgServices()
	if err != nil {
		return nil, err
	}

	if input.Body.SessionID == "" {
		return nil, huma.Error400BadRequest("session_id is required")
	}

	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "api"}
	unbound, err := svc.Bindings.Unbind(ctx, caller, extmsg.UnbindInput{
		Conversation: input.Body.Conversation,
		SessionID:    input.Body.SessionID,
		Now:          time.Now(),
	})
	if err != nil {
		return nil, huma.Error422UnprocessableEntity(err.Error())
	}

	s.extmsgEmitEvent()(events.ExtMsgUnbound, input.Body.SessionID, map[string]any{
		"session_id": input.Body.SessionID,
		"count":      len(unbound),
	})
	raw, _ := json.Marshal(map[string]any{"unbound": unbound})
	out := &ExtMsgUnbindOutput{}
	out.Body = raw
	return out, nil
}

// --- Groups ---

// humaHandleExtMsgGroupLookup is the Huma-typed handler for GET /v0/extmsg/groups.
func (s *Server) humaHandleExtMsgGroupLookup(ctx context.Context, input *ExtMsgGroupLookupInput) (*ExtMsgGroupOutput, error) {
	svc, err := s.humaExtmsgServices()
	if err != nil {
		return nil, err
	}

	ref := extmsg.ConversationRef{
		ScopeID:        input.ScopeID,
		Provider:       input.Provider,
		AccountID:      input.AccountID,
		ConversationID: input.ConversationID,
		Kind:           extmsg.ConversationKind(input.Kind),
	}

	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "api"}
	group, err := svc.Groups.FindByConversation(ctx, caller, ref)
	if err != nil {
		if errors.Is(err, extmsg.ErrGroupNotFound) {
			return nil, huma.Error404NotFound("group not found for conversation")
		}
		return nil, huma.Error500InternalServerError(err.Error())
	}
	raw, _ := json.Marshal(group)
	out := &ExtMsgGroupOutput{}
	out.Body = raw
	return out, nil
}

// humaHandleExtMsgGroupEnsure is the Huma-typed handler for POST /v0/extmsg/groups.
func (s *Server) humaHandleExtMsgGroupEnsure(ctx context.Context, input *ExtMsgGroupEnsureInput) (*ExtMsgGroupEnsureOutput, error) {
	svc, err := s.humaExtmsgServices()
	if err != nil {
		return nil, err
	}

	mode := input.Body.Mode
	if mode == "" {
		mode = extmsg.GroupModeLauncher
	}

	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "api"}
	group, err := svc.Groups.EnsureGroup(ctx, caller, extmsg.EnsureGroupInput{
		RootConversation: input.Body.RootConversation,
		Mode:             mode,
		DefaultHandle:    input.Body.DefaultHandle,
		Metadata:         input.Body.Metadata,
	})
	if err != nil {
		return nil, huma.Error422UnprocessableEntity(err.Error())
	}

	s.extmsgEmitEvent()(events.ExtMsgGroupCreated, group.ID, map[string]any{
		"provider":        input.Body.RootConversation.Provider,
		"conversation_id": input.Body.RootConversation.ConversationID,
		"mode":            string(mode),
	})
	raw, _ := json.Marshal(group)
	out := &ExtMsgGroupEnsureOutput{}
	out.Body = raw
	return out, nil
}

// --- Participants ---

// humaHandleExtMsgParticipantUpsert is the Huma-typed handler for POST /v0/extmsg/participants.
func (s *Server) humaHandleExtMsgParticipantUpsert(ctx context.Context, input *ExtMsgParticipantUpsertInput) (*ExtMsgParticipantOutput, error) {
	svc, err := s.humaExtmsgServices()
	if err != nil {
		return nil, err
	}

	if input.Body.GroupID == "" || input.Body.Handle == "" || input.Body.SessionID == "" {
		return nil, huma.Error400BadRequest("group_id, handle, and session_id are required")
	}

	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "api"}
	participant, err := svc.Groups.UpsertParticipant(ctx, caller, extmsg.UpsertParticipantInput{
		GroupID:   input.Body.GroupID,
		Handle:    input.Body.Handle,
		SessionID: input.Body.SessionID,
		Public:    input.Body.Public,
		Metadata:  input.Body.Metadata,
	})
	if err != nil {
		return nil, huma.Error422UnprocessableEntity(err.Error())
	}
	raw, _ := json.Marshal(participant)
	out := &ExtMsgParticipantOutput{}
	out.Body = raw
	return out, nil
}

// humaHandleExtMsgParticipantRemove is the Huma-typed handler for DELETE /v0/extmsg/participants.
func (s *Server) humaHandleExtMsgParticipantRemove(ctx context.Context, input *ExtMsgParticipantRemoveInput) (*OKResponse, error) {
	svc, err := s.humaExtmsgServices()
	if err != nil {
		return nil, err
	}

	if input.Body.GroupID == "" || input.Body.Handle == "" {
		return nil, huma.Error400BadRequest("group_id and handle are required")
	}

	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "api"}
	err = svc.Groups.RemoveParticipant(ctx, caller, extmsg.RemoveParticipantInput{
		GroupID: input.Body.GroupID,
		Handle:  input.Body.Handle,
	})
	if err != nil {
		return nil, huma.Error422UnprocessableEntity(err.Error())
	}
	out := &OKResponse{}
	out.Body.Status = "removed"
	return out, nil
}

// --- Transcript ---

// humaHandleExtMsgTranscriptList is the Huma-typed handler for GET /v0/extmsg/transcript.
func (s *Server) humaHandleExtMsgTranscriptList(ctx context.Context, input *ExtMsgTranscriptListInput) (*ListOutput[json.RawMessage], error) {
	svc, err := s.humaExtmsgServices()
	if err != nil {
		return nil, err
	}

	ref := extmsg.ConversationRef{
		ScopeID:              input.ScopeID,
		Provider:             input.Provider,
		AccountID:            input.AccountID,
		ConversationID:       input.ConversationID,
		ParentConversationID: input.ParentConversationID,
		Kind:                 extmsg.ConversationKind(input.Kind),
	}

	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "api"}
	entries, err := svc.Transcript.List(ctx, extmsg.ListTranscriptInput{
		Caller:       caller,
		Conversation: ref,
	})
	if err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}
	if entries == nil {
		entries = []extmsg.ConversationTranscriptRecord{}
	}
	rawItems := make([]json.RawMessage, len(entries))
	for i, e := range entries {
		raw, _ := json.Marshal(e)
		rawItems[i] = raw
	}
	return &ListOutput[json.RawMessage]{
		Index: s.latestIndex(),
		Body:  ListBody[json.RawMessage]{Items: rawItems, Total: len(rawItems)},
	}, nil
}

// humaHandleExtMsgTranscriptAck is the Huma-typed handler for POST /v0/extmsg/transcript/ack.
func (s *Server) humaHandleExtMsgTranscriptAck(ctx context.Context, input *ExtMsgTranscriptAckInput) (*OKResponse, error) {
	svc, err := s.humaExtmsgServices()
	if err != nil {
		return nil, err
	}

	if input.Body.SessionID == "" {
		return nil, huma.Error400BadRequest("session_id is required")
	}

	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "api"}
	err = svc.Transcript.Ack(ctx, extmsg.AckMembershipInput{
		Caller:       caller,
		Conversation: input.Body.Conversation,
		SessionID:    input.Body.SessionID,
		Sequence:     input.Body.Sequence,
	})
	if err != nil {
		return nil, huma.Error422UnprocessableEntity(err.Error())
	}
	out := &OKResponse{}
	out.Body.Status = "acked"
	return out, nil
}

// --- Adapters ---

// humaHandleExtMsgAdapterList is the Huma-typed handler for GET /v0/extmsg/adapters.
func (s *Server) humaHandleExtMsgAdapterList(_ context.Context, _ *ExtMsgAdapterListInput) (*ListOutput[json.RawMessage], error) {
	reg, err := s.humaExtmsgAdapterRegistry()
	if err != nil {
		return nil, err
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
	rawItems := make([]json.RawMessage, len(items))
	for i, item := range items {
		raw, _ := json.Marshal(item)
		rawItems[i] = raw
	}
	return &ListOutput[json.RawMessage]{
		Index: s.latestIndex(),
		Body:  ListBody[json.RawMessage]{Items: rawItems, Total: len(rawItems)},
	}, nil
}

// humaHandleExtMsgAdapterRegister is the Huma-typed handler for POST /v0/extmsg/adapters.
func (s *Server) humaHandleExtMsgAdapterRegister(_ context.Context, input *ExtMsgAdapterRegisterInput) (*ExtMsgAdapterRegisterOutput, error) {
	reg, err := s.humaExtmsgAdapterRegistry()
	if err != nil {
		return nil, err
	}

	if input.Body.Provider == "" || input.Body.AccountID == "" {
		return nil, huma.Error400BadRequest("provider and account_id are required")
	}
	name := input.Body.Name
	if name == "" {
		name = input.Body.Provider + "/" + input.Body.AccountID
	}

	adapter := extmsg.NewHTTPAdapter(name, input.Body.CallbackURL, input.Body.Capabilities)
	key := extmsg.AdapterKey{Provider: input.Body.Provider, AccountID: input.Body.AccountID}
	reg.Register(key, adapter)

	s.extmsgEmitEvent()(events.ExtMsgAdapterAdded, name, map[string]any{
		"provider":   input.Body.Provider,
		"account_id": input.Body.AccountID,
	})
	out := &ExtMsgAdapterRegisterOutput{}
	out.Body.Status = "registered"
	out.Body.Provider = input.Body.Provider
	out.Body.AccountID = input.Body.AccountID
	out.Body.Name = name
	return out, nil
}

// humaHandleExtMsgAdapterUnregister is the Huma-typed handler for DELETE /v0/extmsg/adapters.
func (s *Server) humaHandleExtMsgAdapterUnregister(_ context.Context, input *ExtMsgAdapterUnregisterInput) (*OKResponse, error) {
	reg, err := s.humaExtmsgAdapterRegistry()
	if err != nil {
		return nil, err
	}

	if input.Body.Provider == "" || input.Body.AccountID == "" {
		return nil, huma.Error400BadRequest("provider and account_id are required")
	}

	key := extmsg.AdapterKey{Provider: input.Body.Provider, AccountID: input.Body.AccountID}
	reg.Unregister(key)

	s.extmsgEmitEvent()(events.ExtMsgAdapterRemoved, input.Body.Provider+"/"+input.Body.AccountID, map[string]any{
		"provider":   input.Body.Provider,
		"account_id": input.Body.AccountID,
	})
	out := &OKResponse{}
	out.Body.Status = "unregistered"
	return out, nil
}
