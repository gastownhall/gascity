package api

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
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
			// HandleInboundNormalized fails in one of two classes. Permanent
			// rejections (a malformed/unroutable conversation, or an invariant
			// violation such as duplicate active bindings) are a 4xx the adapter
			// should drop: retrying re-resolves the same corrupt state and fails
			// identically, so reporting 5xx would pin the adapter's ordered poll
			// offset behind one poison message and wedge the whole account stream.
			// Transient binding/route/transcript store faults are retryable, so a
			// 5xx the adapter should hold and redeliver. Out-of-process adapters
			// treat 4xx as a permanent drop and 5xx as retryable, so a transient
			// fault must never surface as a permanent 4xx and a permanent fault
			// must never surface as a retryable 5xx. This is a subset of the bind
			// handler's split below: no ErrBindingConflict (409) arm, because the
			// inbound path resolves existing bindings rather than creating them.
			switch {
			// Permanent conditions the normalized path can surface: an
			// unroutable/malformed conversation (ErrInvalidConversation), and an
			// invariant violation (ErrInvariantViolation) from binding, group-route,
			// or transcript resolution — corrupt state that retrying cannot repair,
			// so it is dropped rather than allowed to wedge the stream. The
			// ErrInvalidInput arm is the bind switch's input-validation arm carried
			// over for symmetry; the normalized path hard-codes Kind/Provenance so
			// it has no live ErrInvalidInput source today.
			case errors.Is(handleErr, extmsg.ErrInvalidInput),
				errors.Is(handleErr, extmsg.ErrInvalidConversation),
				errors.Is(handleErr, extmsg.ErrInvariantViolation):
				return nil, huma.Error400BadRequest(handleErr.Error())
			default:
				return nil, huma.Error500InternalServerError(handleErr.Error())
			}
		}
		go s.extmsgNotifyInboundMembers(s.backgroundCtx(), *input.Body.Message)
		out := &ExtMsgInboundOutput{}
		if result != nil {
			out.Body = *result
		}
		return out, nil
	}

	// Raw payload path. Provider and AccountID are only required when
	// Message is nil (the branch above handles the normalized case), so
	// the check stays here rather than in the schema — the schema can't
	// express conditional-on-sibling requiredness cleanly.
	if input.Body.Provider == "" || input.Body.AccountID == "" {
		return nil, huma.Error400BadRequest("provider and account_id are required for raw payloads")
	}

	key := extmsg.AdapterKey{Provider: input.Body.Provider, AccountID: input.Body.AccountID}
	result, err := extmsg.HandleInbound(ctx, deps, key, extmsg.InboundPayload{
		Body:       input.Body.Payload,
		ReceivedAt: time.Now(),
	})
	if err != nil {
		// Unlike the normalized branch above, this raw path maps every failure to
		// a single 422 and does not split transient store faults (5xx) from
		// permanent rejections (4xx). That split is intentionally deferred: the
		// only registered adapter type, HTTPAdapter, rejects raw verification with
		// ErrAdapterUnsupported before any binding/transcript step
		// (internal/extmsg/http_adapter.go), so this branch cannot currently reach
		// a transient storage fault and there is no live data-loss window. A
		// future adapter that actually verifies raw payloads must apply the same
		// errors.Is split used above (4xx for the deterministic adapter/input
		// rejections, 5xx for transient store faults).
		return nil, huma.Error422UnprocessableEntity(err.Error())
	}
	out := &ExtMsgInboundOutput{}
	if result != nil {
		out.Body = *result
	}
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

	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "api"}
	deps := extmsg.OutboundDeps{
		Services:               *svc,
		Registry:               reg,
		EmitEvent:              s.extmsgEmitEvent(),
		ResolveSessionSelector: s.extmsgResolveSessionSelector(),
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
	if result != nil && result.Receipt.Delivered {
		notifyConversation := input.Body.Conversation
		if result.Receipt.Conversation != (extmsg.ConversationRef{}) {
			notifyConversation = result.Receipt.Conversation
		}
		sourceDisplay := s.extmsgSessionHandleForSelector(input.Body.SessionID)
		go s.extmsgNotifyMembers(s.backgroundCtx(), notifyConversation, sourceDisplay, "agent", input.Body.Text, input.Body.SessionID, "")
	}
	out := &ExtMsgOutboundOutput{}
	if result != nil {
		out.Body = *result
	}
	return out, nil
}

// --- Bindings ---

// humaHandleExtMsgBindingList is the Huma-typed handler for GET /v0/extmsg/bindings.
func (s *Server) humaHandleExtMsgBindingList(ctx context.Context, input *ExtMsgBindingListInput) (*ListOutput[extmsg.SessionBindingRecord], error) {
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
	return &ListOutput[extmsg.SessionBindingRecord]{
		Index: s.latestIndex(),
		Body:  ListBody[extmsg.SessionBindingRecord]{Items: bindings, Total: len(bindings)},
	}, nil
}

// humaHandleExtMsgBind is the Huma-typed handler for POST /v0/extmsg/bind.
func (s *Server) humaHandleExtMsgBind(ctx context.Context, input *ExtMsgBindInput) (*ExtMsgBindOutput, error) {
	svc, err := s.humaExtmsgServices()
	if err != nil {
		return nil, err
	}

	// Exactly one of session_id and agent_name — conditional requiredness
	// the schema can't express, enforced here (see ExtMsgInboundInput).
	sessionID := strings.TrimSpace(input.Body.SessionID)
	agentName := strings.TrimSpace(input.Body.AgentName)
	switch {
	case sessionID == "" && agentName == "":
		return nil, huma.Error400BadRequest("session_id or agent_name is required")
	case sessionID != "" && agentName != "":
		return nil, huma.Error400BadRequest("session_id and agent_name are mutually exclusive")
	}
	if agentName != "" {
		// Agent bindings are resolved at delivery time, so the name must
		// map to a configured named-session identity — the only identity
		// the delivery layer can cold-wake a session for. Persist the
		// configured identity so the binding stays unambiguous even when
		// a later config change makes the bare name ambiguous.
		spec, ok, err := s.findNamedSessionSpecForTarget(s.state.CityBeadStore(), agentName)
		if err != nil {
			return nil, huma.Error400BadRequest(fmt.Sprintf("resolving agent %q: %s", agentName, err))
		}
		if !ok {
			return nil, huma.Error400BadRequest(fmt.Sprintf("agent %q does not resolve to a configured named session; agent bindings require a named-session-backed agent", agentName))
		}
		agentName = spec.Identity
	}

	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "api"}
	binding, err := svc.Bindings.Bind(ctx, caller, extmsg.BindInput{
		Conversation: input.Body.Conversation,
		SessionID:    sessionID,
		AgentName:    agentName,
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

	subject := sessionID
	if subject == "" {
		subject = agentName
	}
	s.extmsgEmitEvent()(events.ExtMsgBound, subject, extmsg.BoundEventPayload{
		Provider:       input.Body.Conversation.Provider,
		ConversationID: input.Body.Conversation.ConversationID,
		SessionID:      sessionID,
		AgentName:      agentName,
	})
	out := &ExtMsgBindOutput{}
	out.Body = binding
	return out, nil
}

// humaHandleExtMsgUnbind is the Huma-typed handler for POST /v0/extmsg/unbind.
func (s *Server) humaHandleExtMsgUnbind(ctx context.Context, input *ExtMsgUnbindInput) (*ExtMsgUnbindOutput, error) {
	svc, err := s.humaExtmsgServices()
	if err != nil {
		return nil, err
	}

	sessionID := strings.TrimSpace(input.Body.SessionID)
	agentName := strings.TrimSpace(input.Body.AgentName)
	if input.Body.Conversation == nil && sessionID == "" && agentName == "" {
		return nil, huma.Error400BadRequest("conversation, session_id, or agent_name is required")
	}

	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "api"}
	unbound, err := svc.Bindings.Unbind(ctx, caller, extmsg.UnbindInput{
		Conversation: input.Body.Conversation,
		SessionID:    sessionID,
		AgentName:    agentName,
		Now:          time.Now(),
	})
	if err != nil {
		return nil, huma.Error422UnprocessableEntity(err.Error())
	}

	subject := sessionID
	if subject == "" {
		subject = agentName
	}
	s.extmsgEmitEvent()(events.ExtMsgUnbound, subject, extmsg.UnboundEventPayload{
		SessionID: sessionID,
		Count:     len(unbound),
	})
	out := &ExtMsgUnbindOutput{}
	out.Body = ExtMsgUnbindBody{Unbound: unbound}
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
	out := &ExtMsgGroupOutput{}
	if group != nil {
		out.Body = *group
	}
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

	s.extmsgEmitEvent()(events.ExtMsgGroupCreated, group.ID, extmsg.GroupCreatedEventPayload{
		Provider:       input.Body.RootConversation.Provider,
		ConversationID: input.Body.RootConversation.ConversationID,
		Mode:           string(mode),
	})
	out := &ExtMsgGroupEnsureOutput{}
	out.Body = group
	return out, nil
}

// --- Participants ---

// humaHandleExtMsgParticipantUpsert is the Huma-typed handler for POST /v0/extmsg/participants.
func (s *Server) humaHandleExtMsgParticipantUpsert(ctx context.Context, input *ExtMsgParticipantUpsertInput) (*ExtMsgParticipantOutput, error) {
	svc, err := s.humaExtmsgServices()
	if err != nil {
		return nil, err
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
	out := &ExtMsgParticipantOutput{}
	out.Body = participant
	return out, nil
}

// humaHandleExtMsgParticipantRemove is the Huma-typed handler for DELETE /v0/extmsg/participants.
func (s *Server) humaHandleExtMsgParticipantRemove(ctx context.Context, input *ExtMsgParticipantRemoveInput) (*OKResponse, error) {
	svc, err := s.humaExtmsgServices()
	if err != nil {
		return nil, err
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
func (s *Server) humaHandleExtMsgTranscriptList(ctx context.Context, input *ExtMsgTranscriptListInput) (*ListOutput[extmsg.ConversationTranscriptRecord], error) {
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
		Caller:        caller,
		Conversation:  ref,
		AfterSequence: input.AfterSequence,
		Limit:         input.Limit,
		Order:         extmsg.TranscriptOrder(input.Order),
	})
	if err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}
	if entries == nil {
		entries = []extmsg.ConversationTranscriptRecord{}
	}
	return &ListOutput[extmsg.ConversationTranscriptRecord]{
		Index: s.latestIndex(),
		Body:  ListBody[extmsg.ConversationTranscriptRecord]{Items: entries, Total: len(entries)},
	}, nil
}

// humaHandleExtMsgTranscriptAck is the Huma-typed handler for POST /v0/extmsg/transcript/ack.
func (s *Server) humaHandleExtMsgTranscriptAck(ctx context.Context, input *ExtMsgTranscriptAckInput) (*OKResponse, error) {
	svc, err := s.humaExtmsgServices()
	if err != nil {
		return nil, err
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

// extmsgAdapterInfo is the response shape for each entry in GET /v0/extmsg/adapters.
type extmsgAdapterInfo struct {
	Provider  string `json:"provider" doc:"Adapter provider key."`
	AccountID string `json:"account_id" doc:"Adapter account ID."`
	Name      string `json:"name" doc:"Adapter display name."`
}

// humaHandleExtMsgAdapterList is the Huma-typed handler for GET /v0/extmsg/adapters.
func (s *Server) humaHandleExtMsgAdapterList(_ context.Context, _ *ExtMsgAdapterListInput) (*ListOutput[extmsgAdapterInfo], error) {
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
	items := make([]extmsgAdapterInfo, 0, len(keys))
	for _, k := range keys {
		a := reg.Lookup(k)
		name := ""
		if a != nil {
			name = a.Name()
		}
		items = append(items, extmsgAdapterInfo{
			Provider:  k.Provider,
			AccountID: k.AccountID,
			Name:      name,
		})
	}
	return &ListOutput[extmsgAdapterInfo]{
		Index: s.latestIndex(),
		Body:  ListBody[extmsgAdapterInfo]{Items: items, Total: len(items)},
	}, nil
}

// humaHandleExtMsgAdapterRegister is the Huma-typed handler for POST /v0/extmsg/adapters.
func (s *Server) humaHandleExtMsgAdapterRegister(_ context.Context, input *ExtMsgAdapterRegisterInput) (*ExtMsgAdapterRegisterOutput, error) {
	reg, err := s.humaExtmsgAdapterRegistry()
	if err != nil {
		return nil, err
	}

	name := input.Body.Name
	if name == "" {
		name = input.Body.Provider + "/" + input.Body.AccountID
	}

	adapter := extmsg.NewHTTPAdapter(name, input.Body.CallbackURL, input.Body.Capabilities)
	key := extmsg.AdapterKey{Provider: input.Body.Provider, AccountID: input.Body.AccountID}
	reg.Register(key, adapter)

	s.extmsgEmitEvent()(events.ExtMsgAdapterAdded, name, extmsg.AdapterEventPayload{
		Provider:  input.Body.Provider,
		AccountID: input.Body.AccountID,
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

	key := extmsg.AdapterKey{Provider: input.Body.Provider, AccountID: input.Body.AccountID}
	reg.Unregister(key)

	s.extmsgEmitEvent()(events.ExtMsgAdapterRemoved, input.Body.Provider+"/"+input.Body.AccountID, extmsg.AdapterEventPayload{
		Provider:  input.Body.Provider,
		AccountID: input.Body.AccountID,
	})
	out := &OKResponse{}
	out.Body.Status = "unregistered"
	return out, nil
}
