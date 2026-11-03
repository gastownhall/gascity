package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/session"
)

// ExtMsgPublicState is the optional typed admission seam. The controller owns
// Messaging persistence; the API never mines another class's physical backend.
type ExtMsgPublicState interface{ ExtMsgPublicStore() beads.MailStore }

// ExtMsgPublicLifecycle owns the background relay. Only a writable production
// city with a cancellable controller lifetime starts one; read-only API states
// and ordinary test states start no mutator goroutines.
type ExtMsgPublicLifecycle interface{ ExtMsgPublicContext() context.Context }

type extmsgPublicLane struct{ s *Server }

func (l extmsgPublicLane) store() (*opjournal.Store, error) { return l.openStore(true) }

func (l extmsgPublicLane) openStore(requireHost bool) (*opjournal.Store, error) {
	if requireHost && !l.s.laneExecutes(opjournal.KindMessage) {
		return nil, apierr.OpJournalUnavailable.Msg("public delivery requires a running message conversation lane")
	}
	over := l.s.state.SessionsBeadStore()
	prefix, ok := beads.As[beads.IDPrefixReporter](over)
	if !ok || prefix.IDPrefix() == "" {
		return nil, apierr.OpJournalUnavailable.Msg("Sessions writer has no ID namespace")
	}
	// NewLaneStore checks the actual writer's guarded-batch and conditional-write
	// capabilities. A CLI writer with a native read cache does not pass.
	st, err := opjournal.NewLaneStore(over, clock.Real{}, opjournal.WithIDPrefix(prefix.IDPrefix()))
	if err != nil {
		return nil, apierr.OpJournalUnavailable.Msg("opening public Sessions lane: " + err.Error())
	}
	return st, nil
}

func (l extmsgPublicLane) Ready(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, ok := l.s.state.EventProvider().(events.AckRecorder); !ok {
		return apierr.OpJournalUnavailable.Msg("public invalidation recorder cannot acknowledge events")
	}
	_, err := l.store()
	return err
}

func (l extmsgPublicLane) ResolveRecipient(ctx context.Context, r extmsg.PublicRecipient) (string, error) {
	store := l.s.state.SessionsBeadStore()
	if _, ok := beads.As[beads.IDPrefixReporter](store); !ok {
		return "", apierr.OpJournalUnavailable.Msg("Sessions store unavailable")
	}
	var id string
	var err error
	if r.SessionID != "" {
		id, err = l.s.resolveSessionTargetIDWithContext(ctx, store, r.SessionID, apiSessionResolveOptions{})
	} else {
		id, err = l.s.resolveSessionIDMaterializingNamedWithContext(ctx, store, r.AgentIdentity)
	}
	if publicRecipientMissing(err) {
		return "", &extmsg.PublicDeliveryError{Code: "target_gone", Cause: err}
	}
	return id, err
}

// Prepare freezes the complete native request before the first lane write.
// A changed handle/config/formatter after a lost acknowledgment cannot change
// the request under the already-committed operation key.
func (l extmsgPublicLane) Prepare(ctx context.Context, d extmsg.PublicDelivery, r extmsg.ConversationTranscriptRecord) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	st, err := l.store()
	if err != nil {
		return "", err
	}
	info, err := session.NewStore(l.s.state.SessionsBeadStore()).Get(d.ResolvedSessionID)
	if publicRecipientMissing(err) {
		return "", &extmsg.PublicDeliveryError{Code: "target_gone", Cause: err}
	}
	if err != nil {
		return "", err
	}
	if info.Closed {
		return "", &extmsg.PublicDeliveryError{Code: "target_closed", Cause: fmt.Errorf("session %s is closed", d.ResolvedSessionID)}
	}
	actorKind := "human"
	if r.Actor.IsBot {
		actorKind = "agent"
	}
	selector := d.Recipient.SessionID
	if selector == "" {
		selector = d.Recipient.AgentIdentity
	}
	message := l.s.extmsgNotice(extmsgNotifyReminder{
		Provider: r.Conversation.Provider, ConversationID: r.Conversation.ConversationID,
		ActorDisplay: r.Actor.DisplayName, ActorKind: actorKind, Text: r.Text,
		RecipientSelector: selector, RecipientSessionID: d.ResolvedSessionID,
		Handle:         l.s.extmsgSessionHandleForResolvedID(d.ResolvedSessionID, selector),
		ExplicitTarget: r.ExplicitTarget, ExplicitTargetSessionID: d.ExplicitTargetSessionID,
	})
	req := opjournal.Request{
		Conversation: d.ResolvedSessionID, Kind: opjournal.KindMessage, Executor: opjournal.ExecutorLane,
		ClientKey: d.OperationKey, Producer: opjournal.Producer{Principal: extmsgNoticePrincipal, Actor: opjournal.ActorBackground},
		Payload: message, Policy: opjournal.Policy{Guarantee: opjournal.GuaranteeAtMostOnce, Message: opjournal.MessagePolicy(d.Recipient.Policy)},
		Causation: d.TranscriptID, Correlation: d.ReceiptID,
	}
	if _, err := st.Preview(req); err != nil {
		if errors.Is(err, opjournal.ErrInvalidRequest) || errors.Is(err, opjournal.ErrPayloadTooLarge) {
			return "", &extmsg.PublicDeliveryError{Code: "invalid_native_payload", Cause: err}
		}
		return "", err
	}
	encoded, err := json.Marshal(req)
	return string(encoded), err
}

func (l extmsgPublicLane) Admit(ctx context.Context, d extmsg.PublicDelivery, _ extmsg.ConversationTranscriptRecord) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	st, err := l.store()
	if err != nil {
		return "", err
	}
	req, err := publicFrozenLaneRequest(d)
	if err != nil {
		return "", err
	}
	op, _, err := st.Propose(req)
	if err != nil {
		return "", proposeError(err)
	}
	// A previous write may have committed without its acknowledgment/poke.
	// Re-poking the same operation is safe; it never admits another native turn.
	l.s.pokeConversationLane(req.Conversation)
	return op.ID, nil
}

func publicFrozenLaneRequest(d extmsg.PublicDelivery) (opjournal.Request, error) {
	var req opjournal.Request
	if err := json.Unmarshal([]byte(d.NativePayload), &req); err != nil {
		return req, err
	}
	if req.ClientKey != d.OperationKey || req.Conversation != d.ResolvedSessionID ||
		req.Causation != d.TranscriptID || req.Correlation != d.ReceiptID ||
		req.Kind != opjournal.KindMessage || req.Executor != opjournal.ExecutorLane ||
		req.Producer.Principal != extmsgNoticePrincipal || req.Producer.Actor != opjournal.ActorBackground ||
		req.Policy.Guarantee != opjournal.GuaranteeAtMostOnce || req.Policy.Message != opjournal.MessagePolicy(d.Recipient.Policy) {
		return req, fmt.Errorf("%w: invalid frozen public lane request", extmsg.ErrInvariantViolation)
	}
	return req, nil
}

func (l extmsgPublicLane) Cancel(ctx context.Context, d extmsg.PublicDelivery) (extmsg.PublicCancelResult, error) {
	st, err := l.openStore(false)
	if err != nil {
		return extmsg.PublicCancelResult{}, err
	}
	req, err := publicFrozenLaneRequest(d)
	if err != nil {
		return extmsg.PublicCancelResult{}, err
	}
	_, op, err := st.CancelProposal(ctx, req, extmsgNoticePrincipal, "public delivery canceled")
	result := extmsg.PublicCancelResult{
		OperationID: op.ID, Status: string(op.Status), Outcome: string(op.Outcome.Arm), Reason: op.Outcome.Reason,
		Terminal: op.Status == opjournal.StatusClosed, Canceled: op.Status == opjournal.StatusClosed && op.Outcome.Arm == opjournal.ArmCanceled,
	}
	if op.ID != "" {
		l.s.pokeConversationLane(req.Conversation)
	}
	return result, err
}

func (s *Server) publicAdmissionStore() (*extmsg.PublicAdmissionStore, error) {
	source, ok := s.state.(ExtMsgPublicState)
	if !ok {
		return nil, apierr.OpJournalUnavailable.Msg("public admission writer is unavailable")
	}
	over := source.ExtMsgPublicStore()
	prefix, ok := beads.As[beads.IDPrefixReporter](over)
	if !ok {
		return nil, apierr.OpJournalUnavailable.Msg("Messaging writer has no ID namespace")
	}
	store, err := extmsg.NewPublicAdmissionStore(over, prefix.IDPrefix(), extmsgPublicLane{s: s})
	if err != nil {
		return nil, apierr.OpJournalUnavailable.Msg("opening public admission: " + err.Error())
	}
	return store, nil
}

// validatePublicRecipients is NEW admission validation only. A durable retry
// first recovers its frozen receipt, even if membership or config later changes.
// Concrete sessions must exist in this city; named identities must resolve to
// this city's configuration. Each must be a current member or binding target.
func (s *Server) publicRecipientKey(ctx context.Context, selector string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	store := s.state.SessionsBeadStore()
	spec, named, err := s.findNamedSessionSpecForTarget(store, selector)
	if err != nil {
		return "", err
	}
	if named {
		return "agent:" + spec.Identity, nil
	}
	id, err := s.resolveSessionTargetIDWithContext(ctx, store, selector, apiSessionResolveOptions{})
	if err != nil {
		return "", err
	}
	info, err := session.NewStore(store).Get(id)
	if err != nil {
		return "", err
	}
	if identity := strings.TrimSpace(info.ConfiguredNamedIdentity); identity != "" {
		spec, ok, err := s.findNamedSessionSpecForTarget(store, identity)
		if err != nil {
			return "", err
		}
		if ok {
			canonical, found, err := s.findCanonicalNamedSession(store, spec)
			if err != nil {
				return "", err
			}
			if found && canonical.ID == id {
				return "agent:" + spec.Identity, nil
			}
		}
	}
	return "session:" + id, nil
}

func publicRecipientMissing(err error) bool {
	return errors.Is(err, session.ErrSessionNotFound) || errors.Is(err, beads.ErrNotFound)
}

func (s *Server) validatePublicRecipients(ctx context.Context, svc *extmsg.Services, msg extmsg.ExternalInboundMessage, recipients []extmsg.PublicRecipient) (map[string]string, error) {
	if len(recipients) == 0 {
		return nil, apierr.InvalidRequest.Msg("durable public admission requires explicit deliveries")
	}
	members, err := svc.Transcript.ListMemberships(ctx, extmsg.Caller{Kind: extmsg.CallerController, ID: "public-admission"}, msg.Conversation)
	if err != nil {
		return nil, err
	}
	allowed := make(map[string]bool)
	add := func(selector string) error {
		if strings.TrimSpace(selector) == "" {
			return nil
		}
		key, err := s.publicRecipientKey(ctx, selector)
		if publicRecipientMissing(err) {
			return nil
		}
		if err != nil {
			return err
		}
		allowed[key] = true
		return nil
	}
	for _, m := range members {
		if err := add(m.SessionID); err != nil {
			return nil, err
		}
	}
	binding, err := svc.Bindings.ResolveByConversation(ctx, msg.Conversation)
	if err != nil {
		return nil, err
	}
	if binding != nil {
		if err := add(binding.SessionID); err != nil {
			return nil, err
		}
		if err := add(binding.AgentName); err != nil {
			return nil, err
		}
	}
	identities := make(map[string]string)
	seen := make(map[string]bool)
	store := s.state.SessionsBeadStore()
	for _, r := range recipients {
		selector := r.SessionID
		if selector == "" {
			spec, ok, err := s.findNamedSessionSpecForTarget(store, r.AgentIdentity)
			if err != nil {
				return nil, err
			}
			if !ok || spec.Identity != r.AgentIdentity {
				return nil, apierr.InvalidRequest.Msg("public delivery agent is not a configured named identity")
			}
			selector = r.AgentIdentity
		} else {
			resolved, err := s.resolveSessionTargetIDWithContext(ctx, store, selector, apiSessionResolveOptions{})
			if err != nil && !publicRecipientMissing(err) {
				return nil, err
			}
			if err != nil || resolved != selector {
				return nil, apierr.InvalidRequest.Msg("public delivery session is not a concrete session of this city")
			}
		}
		key, err := s.publicRecipientKey(ctx, selector)
		if err != nil && !publicRecipientMissing(err) {
			return nil, err
		}
		if err != nil || !allowed[key] {
			return nil, apierr.InvalidRequest.Msg("public delivery target is not a member of this conversation")
		}
		if seen[key] {
			return nil, apierr.InvalidRequest.Msg("duplicate public recipient through named/session aliases")
		}
		seen[key] = true
		identities[r.SessionID+"\x00"+r.AgentIdentity] = key
	}
	return identities, nil
}

// publicInboundRoute snapshots read-only binding/group routing for a NEW
// admission. Explicit deliveries remain the only delivery selector; this does
// not create a default binding or re-evaluate routing after a receipt exists.
func (s *Server) publicInboundRoute(ctx context.Context, svc *extmsg.Services, message extmsg.ExternalInboundMessage) (extmsg.PublicInboundRoute, error) {
	var frozen extmsg.PublicInboundRoute
	binding, err := svc.Bindings.ResolveByConversation(ctx, message.Conversation)
	if err != nil {
		return frozen, err
	}
	if binding != nil {
		frozen.TargetSessionID = binding.SessionID
		frozen.TargetAgentName = binding.AgentName
	}
	route, err := svc.Groups.ResolveInbound(ctx, message)
	if err != nil {
		if errors.Is(err, extmsg.ErrGroupNotFound) || errors.Is(err, extmsg.ErrGroupRouteNotFound) {
			return frozen, nil
		}
		return frozen, err
	}
	if route != nil {
		if route.Match == extmsg.GroupRouteExplicitTarget {
			frozen.ExplicitTargetSessionID = strings.TrimSpace(route.TargetSessionID)
		}
		if frozen.TargetSessionID == "" && frozen.TargetAgentName == "" {
			frozen.TargetSessionID = strings.TrimSpace(route.TargetSessionID)
		}
	}
	return frozen, nil
}

func publicAdmissionError(err error) error {
	var problem *apierr.ErrorModel
	if errors.As(err, &problem) {
		return err
	}
	switch {
	case errors.Is(err, extmsg.ErrMessageConflict):
		return apierr.ConflictConcurrentModify.Msg(err.Error())
	case errors.Is(err, extmsg.ErrInvalidInput), errors.Is(err, extmsg.ErrInvalidConversation), errors.Is(err, opjournal.ErrInvalidRequest):
		return apierr.InvalidRequest.Msg(err.Error())
	default:
		return apierr.OpJournalUnavailable.Msg("public admission unavailable: " + err.Error())
	}
}

func (s *Server) admitExtMsgPublic(ctx context.Context, svc *extmsg.Services, message extmsg.ExternalInboundMessage, key string, recipients []extmsg.PublicRecipient, cancelDeliveries bool) (*ExtMsgInboundOutput, error) {
	store, err := s.publicAdmissionStore()
	if err != nil {
		return nil, err
	}
	input := extmsg.PublicAdmissionInput{Caller: extmsg.Caller{Kind: extmsg.CallerController, ID: "api"}, RequestKey: key, Message: message, Recipients: recipients}
	receipt, found, err := store.Recover(ctx, input)
	if err != nil {
		return nil, publicAdmissionError(err)
	}
	if cancelDeliveries && !found {
		return nil, apierr.RequestNotFound.Msg("public request has not been admitted; cancellation created nothing")
	}
	if !found {
		if err := store.Ready(ctx); err != nil {
			return nil, publicAdmissionError(err)
		}
		input.RecipientKeys, err = s.validatePublicRecipients(ctx, svc, message, recipients)
		if err != nil {
			return nil, publicAdmissionError(err)
		}
		input.Route, err = s.publicInboundRoute(ctx, svc, message)
		if err != nil {
			return nil, publicAdmissionError(err)
		}
		receipt, err = store.Admit(ctx, input)
		if err != nil {
			return nil, publicAdmissionError(err)
		}
	}
	if cancelDeliveries {
		receipt, err = store.RequestCancel(ctx, receipt.ID)
		if err != nil {
			return nil, publicAdmissionError(err)
		}
	}
	record, deliveries, err := store.ReadAdmission(ctx, receipt)
	if err != nil {
		return nil, publicAdmissionError(err)
	}
	// A synchronous response proves only atomic Messaging admission. The durable
	// outbox is relayed by the city's worker, not an untracked request goroutine.
	out := &ExtMsgInboundOutput{}
	out.Body = extmsg.InboundResult{Message: message, TranscriptEntry: &record, Admission: &receipt, Deliveries: deliveries}
	return out, nil
}

func (s *Server) relayExtMsgPublic(ctx context.Context) error {
	store, err := s.publicAdmissionStore()
	if err != nil {
		return err
	}
	var errs []error
	var eventAfter *beads.SeekBoundary
	for {
		pending, next, err := store.PendingInvalidations(ctx, 100, eventAfter)
		if err != nil {
			errs = append(errs, err)
			break
		}
		for _, receipt := range pending {
			err := store.Invalidate(ctx, receipt.ID, s.emitPublicInvalidation)
			if err != nil {
				errs = append(errs, err)
			}
		}
		if next == nil {
			break
		}
		eventAfter = next
	}
	var after *beads.SeekBoundary
	for {
		pending, next, err := store.PendingPage(ctx, 100, after)
		if err != nil {
			return err
		}
		for _, d := range pending {
			if _, err := store.Relay(ctx, d.ID); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", d.ID, err))
			}
		}
		if next == nil {
			break
		}
		after = next
	}

	var changeAfter *beads.SeekBoundary
	for {
		pending, next, err := store.PendingDeliveryChanges(ctx, 100, changeAfter)
		if err != nil {
			errs = append(errs, err)
			break
		}
		for _, d := range pending {
			if err := store.InvalidateDelivery(ctx, d.ID, s.emitPublicDeliveryChange); err != nil {
				errs = append(errs, err)
			}
		}
		if next == nil {
			break
		}
		changeAfter = next
	}
	return errors.Join(errs...)
}

func (s *Server) startExtMsgPublicRelay(owner context.Context) <-chan struct{} {
	if s.readOnly {
		return nil
	}
	lifecycle, ok := s.state.(ExtMsgPublicLifecycle)
	if !ok {
		return nil
	}
	if _, ok := s.state.(ExtMsgPublicState); !ok {
		return nil
	}
	cityCtx := lifecycle.ExtMsgPublicContext()
	if cityCtx == nil || cityCtx.Done() == nil {
		return nil
	}
	ctx, cancel := context.WithCancel(owner)
	stopCity := context.AfterFunc(cityCtx, cancel)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cancel()
		defer stopCity()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		lastError := ""
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if s.liveServer() != s {
					return
				}
				err := s.relayExtMsgPublic(ctx)
				current := ""
				if err != nil {
					current = err.Error()
				}
				if current != lastError && current != "" {
					log.Printf("extmsg public relay: %s", current)
				}
				if current == "" && lastError != "" {
					log.Printf("extmsg public relay: recovered")
				}
				lastError = current
			}
		}
	}()
	return done
}

// Keep whitespace semantics consistent at the HTTP edge, before key dispatch.
func durablePublicKey(key string) string { return strings.TrimSpace(key) }

// emitPublicInvalidation retains extmsg.inbound routing fields and adds stable
// public identities. Notification is at-least-once across a crash or lost ack;
// the receipt/transcript/op are still individually admitted only once.
func (s *Server) emitPublicInvalidation(receipt extmsg.PublicReceipt, record extmsg.ConversationTranscriptRecord) error {
	recorder, ok := s.state.EventProvider().(events.AckRecorder)
	if !ok {
		return apierr.OpJournalUnavailable.Msg("public invalidation recorder cannot acknowledge events")
	}
	payload := extmsg.InboundEventPayload{
		Provider: record.Conversation.Provider, ConversationID: record.Conversation.ConversationID,
		Actor: record.Actor.DisplayName, AdmissionID: receipt.ID, TranscriptID: record.ID,
	}
	payload.TargetSession = receipt.TargetSession
	payload.TargetAgent = receipt.TargetAgent
	subject := receipt.ID
	if payload.TargetSession != "" {
		subject = payload.TargetSession
	} else if payload.TargetAgent != "" {
		subject = payload.TargetAgent
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return recorder.RecordAck(events.Event{Type: events.ExtMsgInbound, Subject: subject, Payload: encoded})
}

func (s *Server) emitPublicDeliveryChange(d extmsg.PublicDelivery) error {
	recorder, ok := s.state.EventProvider().(events.AckRecorder)
	if !ok {
		return apierr.OpJournalUnavailable.Msg("public delivery recorder cannot acknowledge events")
	}
	payload := extmsg.PublicDeliveryChangedPayload{
		AdmissionID: d.ReceiptID, TranscriptID: d.TranscriptID, DeliveryID: d.ID, StateVersion: d.StateVersion,
		State: d.State, RecipientKey: d.RecipientKey, SessionID: d.ResolvedSessionID, OperationID: d.OperationID,
		ErrorCode: d.ErrorCode, Retryable: d.Retryable, CancelRequested: d.CancelRequested,
		OperationStatus: d.OperationStatus, OperationOutcome: d.OperationOutcome, OperationReason: d.OperationReason,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return recorder.RecordAck(events.Event{Type: events.ExtMsgPublicDeliveryChanged, Subject: d.ID, Payload: encoded})
}
