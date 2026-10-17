package api

// The API's O0 shadow ops on the content verbs, POST .../session/{id}/submit
// and .../messages (engdocs/design/des-o0-api-shadow-ops.md §6; DES-O0API
// below).
//
// Under the ops.content rollout gate each request is recorded as a message
// op before the legacy delivery runs and closed from the legacy result after
// it, through the legacyop front door. Off records nothing and answers
// byte for byte as before. Auto records where the op store can and lets the
// request run unrecorded where it cannot. Require refuses a request it cannot
// record before any effect. The existing Idempotency-Key LRU stays in front,
// unchanged; behind it the in-flight registry and the op row are the dedupe
// authority, so a retry finds its request again across a restart, across
// /submit and /messages, and across alias and bead id. The delivered text is
// the legacy path's, unchanged: no op id, key or marker reaches a runtime
// (D15, I11).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/opjournal/legacyop"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/telemetry"
)

// messageSentinels maps the message path's legacy errors onto op outcomes
// (DES-O0API §6.3). The delivery timeout of /messages is reported as
// context.DeadlineExceeded.
var messageSentinels = legacyop.Sentinels{
	Unsupported:         []error{session.ErrTurnInterruptUnsupported},
	ConversationClosed:  []error{session.ErrSessionClosed},
	SessionGone:         []error{session.ErrSessionNotFound},
	ProviderUnavailable: []error{runtime.ErrRuntimeUnavailable},
	DeliveryTimeout:     []error{context.DeadlineExceeded, context.Canceled},
}

// messageTimeoutError is /messages' delivery timeout: the legacy event text,
// classified as context.DeadlineExceeded for the op outcome.
type messageTimeoutError struct{}

// Error is the legacy timeout text.
func (messageTimeoutError) Error() string {
	return fmt.Sprintf("session.message timed out after %s", sessionMessageAsyncTimeout)
}

// Unwrap classifies the timeout as a deadline.
func (messageTimeoutError) Unwrap() error { return context.DeadlineExceeded }

// opTargetMovedResult counts a recorded message whose target re-resolved to
// another session between the 202 and the delivery (DES-O0API §6.1).
const opTargetMovedResult = "op_target_moved"

// serverOps is one Server's operation-journal state: the in-flight registry,
// the content recorder and the lazily opened op store.
type serverOps struct {
	inflight *legacyop.Inflight
	content  *legacyop.Recorder
	clk      clock.Clock
	// openStore opens the op store over a sessions-class store; tests
	// substitute it.
	openStore func(beads.SessionStore, clock.Clock) (*opjournal.Store, error)

	mu   sync.Mutex
	st   *opjournal.Store
	over beads.Store
}

// newServerOps builds s's op state over its boot-latched gates. It runs once
// at Server construction, after the gates are latched.
func newServerOps(s *Server) *serverOps {
	o := &serverOps{
		inflight: legacyop.NewInflight(),
		clk:      clock.Real{},
		openStore: func(st beads.SessionStore, clk clock.Clock) (*opjournal.Store, error) {
			return opjournal.NewStore(st, clk)
		},
	}
	degraded := NewOpsDegradeReporter(func() events.Recorder { return s.state.EventProvider() }, o.clk)
	o.content = &legacyop.Recorder{
		Mode:     s.bootFlags.OpsContent(),
		Executor: legacyop.ExecutorAPI,
		Open:     func() (*opjournal.Store, error) { return o.store(s.state.SessionsBeadStore()) },
		Inflight: o.inflight,
		Clock:    o.clk,
		Degraded: func(d legacyop.Degrade) { degraded.Report(rollout.KeyOpsContent, d) },
	}
	return o
}

// store returns the cached op store, opening it over over when none is cached
// or the sessions-class store was replaced (a reload).
func (o *serverOps) store(over beads.SessionStore) (*opjournal.Store, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.st != nil && sameStoreValue(o.over, over.Store) {
		return o.st, nil
	}
	st, err := o.openStore(over, o.clk)
	if err != nil {
		o.st, o.over = nil, nil
		return nil, err
	}
	o.st, o.over = st, over.Store
	return st, nil
}

// cached returns the op store last opened, or nil.
func (o *serverOps) cached() *opjournal.Store {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.st
}

// sameStoreValue reports whether a and b are the same store value; a store
// type that is not comparable reads as different, so the op store reopens.
func sameStoreValue(a, b beads.Store) (same bool) {
	defer func() {
		if recover() != nil {
			same = false
		}
	}()
	return a == b
}

// messageOpRequest is one /submit or /messages request to record.
type messageOpRequest struct {
	// target is the session as addressed; conv its resolved id, "" for a
	// configured named session with no row yet.
	target string
	conv   string
	// key is the Idempotency-Key header, "" when absent.
	key    string
	text   string
	intent session.SubmitIntent
	// operation is the endpoint's request operation, the one its terminal
	// events name.
	operation string
}

// admitMessageOp admits req under the ops.content gate (DES-O0API §6.1 steps
// 3 and 4). It returns either an admission to execute or the body to answer
// a replay with (DES-O0API §6.2), or a refusal mapped to the wire. cursor is
// the event cursor captured before admission: a stored replay re-emits its
// terminal event after it.
func (s *Server) admitMessageOp(ctx context.Context, store beads.Store, req *messageOpRequest, cursor string) (*legacyop.Admission, *asyncAcceptedBody, error) {
	rec := s.ops.content
	if req.conv == "" {
		if rec.Mode != gate.Require {
			return rec.Unrecorded(opjournal.KindMessage, req.key, legacyop.ReasonUnmaterializedTarget), nil, nil
		}
		// Require: materialize the named session first, as /wake does, so
		// the op names its conversation (DES-O0API §6.4).
		id, err := s.resolveSessionIDMaterializingNamedWithContext(ctx, store, req.target)
		if err != nil {
			return nil, nil, humaResolveError(err)
		}
		req.conv = id
	}
	adm, err := rec.Admit(legacyop.Request{
		Conversation: req.conv,
		Principal:    legacyop.PrincipalOperator,
		Actor:        opjournal.ActorOperator,
		Key:          req.key,
		Payload:      req.text,
		Kind:         opjournal.KindMessage,
		Policy:       messageOpPolicy(req.intent),
	})
	if err != nil {
		return nil, nil, opAdmitError(err, req.key)
	}
	if adm.Decision != legacyop.Replay {
		return adm, nil, nil
	}
	if adm.Cached != nil {
		body, ok := adm.Cached.(asyncAcceptedBody)
		if !ok {
			return nil, nil, apierr.IdempotencyInFlight.Msg("in_flight: request with this Idempotency-Key is already in progress")
		}
		return nil, &body, nil
	}
	body := asyncAcceptedBody{
		Status:         "accepted",
		RequestID:      adm.Op.ID,
		EventCursor:    cursor,
		IdempotencyKey: adm.Key,
		Replayed:       true,
		Op:             conversationOpWire(adm.Op),
	}
	s.emitMessageOpProjection(req.operation, adm.Op)
	return nil, &body, nil
}

// messageOpPolicy is the message policy of a submit intent (DES-O0API §3.1);
// /messages is the default intent.
func messageOpPolicy(intent session.SubmitIntent) opjournal.Policy {
	when := opjournal.WhenBoundary
	switch intent {
	case session.SubmitIntentFollowUp:
		when = opjournal.WhenAfterTurn
	case session.SubmitIntentInterruptNow:
		when = opjournal.WhenInterrupt
	}
	return opjournal.Policy{Message: opjournal.MessagePolicy{When: when, Wake: opjournal.WakeIfNeeded, Role: opjournal.RolePrompt}}
}

// submitIntentForWhen is the submit intent a recorded message policy came
// from, for a replay's projection.
func submitIntentForWhen(when opjournal.When) session.SubmitIntent {
	switch when {
	case opjournal.WhenAfterTurn:
		return session.SubmitIntentFollowUp
	case opjournal.WhenInterrupt:
		return session.SubmitIntentInterruptNow
	default:
		return session.SubmitIntentDefault
	}
}

// opAdmitError maps an Admit refusal onto the wire (DES-O0API §10.3). The
// in-flight and mismatch answers reuse the LRU's codes.
func opAdmitError(err error, key string) error {
	var busy *legacyop.BusyError
	switch {
	case errors.As(err, &busy):
		return apierr.IdempotencyInFlight.Msg("in_flight: request with this Idempotency-Key is already in progress")
	case errors.Is(err, opjournal.ErrKeyReuse):
		return apierr.IdempotencyMismatch.Msg("idempotency_mismatch: Idempotency-Key reused with a different request")
	case errors.Is(err, legacyop.ErrUnavailable):
		return apierr.OpJournalUnavailable.Msg("op_journal_unavailable: the operation journal cannot record this request; nothing was delivered: " + err.Error())
	case errors.Is(err, opjournal.ErrPayloadTooLarge):
		return apierr.PayloadTooLarge.Msg(fmt.Sprintf("payload_too_large: the message exceeds the operation journal's %d-byte limit", opjournal.MaxInlinePayload))
	case errors.Is(err, opjournal.ErrInvalidRequest) && key != "" && opjournal.ValidateClientKey(key) != nil:
		return apierr.InvalidIdempotencyKey.Msg("invalid_idempotency_key: " + err.Error())
	case errors.Is(err, opjournal.ErrInvalidRequest):
		return apierr.ValidationFailed.Msg("invalid_request: " + err.Error())
	default:
		return apierr.Internal.Msg("recording the request: " + err.Error())
	}
}

// acceptedMessageBody is the 202 body of an admitted request: the legacy
// body, plus the key and the op when the request was recorded.
func acceptedMessageBody(adm *legacyop.Admission, requestID, cursor string) asyncAcceptedBody {
	body := asyncAcceptedBody{Status: "accepted", RequestID: requestID, EventCursor: cursor}
	if adm.Recorded {
		body.IdempotencyKey = adm.Key
		body.Op = conversationOpWire(adm.Op)
	}
	return body
}

// messageRequestID is the request_id of an admitted request: the op id when
// it was recorded, else a fresh correlation id (DES-O0API §10.1).
func messageRequestID(adm *legacyop.Admission) (string, error) {
	if adm.Recorded {
		return adm.Op.ID, nil
	}
	return newRequestID()
}

// messageOpResult is the op result of a message delivery's legacy result. A
// delivery whose interrupt the runtime accepted unfenced records that
// degradation.
func messageOpResult(queued bool, err error, interrupt *session.TurnInterruptReport) legacyop.Result {
	res := messageSentinels.MessageResult(queued, err)
	if err == nil && interrupt != nil && interrupt.Degradation != "" {
		res.Outcome.Degradation = interrupt.Degradation
	}
	return res
}

// finishMessageOp closes adm from res, before the terminal event is emitted
// (I10), and returns the op that event carries: nil when the request was not
// recorded. A close that keeps failing retries in the background; in require
// the event then waits for that retry to end, in auto it is emitted at once
// with the op still open (DES-O0API §6.1 step 8).
func (s *Server) finishMessageOp(adm *legacyop.Admission, res legacyop.Result) *ConversationOp {
	if adm == nil {
		return nil
	}
	f := adm.Finish(res)
	if !adm.Recorded {
		return nil
	}
	if f.Closed {
		return conversationOpWire(f.Op)
	}
	if f.Background != nil && s.ops.content.Mode == gate.Require {
		<-f.Background
	}
	return s.currentOpWire(adm.Op)
}

// currentOpWire re-reads op for the wire, falling back to the copy held when
// the read fails.
func (s *Server) currentOpWire(op opjournal.Op) *ConversationOp {
	if st := s.ops.cached(); st != nil {
		if cur, err := st.Get(op.ID); err == nil {
			return conversationOpWire(cur)
		}
	}
	return conversationOpWire(op)
}

// noteTargetMoved counts a recorded message whose target re-resolved to
// another session than the one its op names; the op still closes from the
// legacy result (DES-O0API §6.1).
func noteTargetMoved(adm *legacyop.Admission, delivered string) {
	if adm == nil || !adm.Recorded || delivered == adm.Op.Conversation {
		return
	}
	log.Printf("api: op %s for session %s was delivered to %s: the target moved after the 202", adm.Op.ID, adm.Op.Conversation, delivered)
	telemetry.RecordOpRecord(context.Background(), string(opjournal.KindMessage), legacyop.SurfaceAPI, legacyop.StageClose, opTargetMovedResult, "")
}

// emitMessageOpProjection re-emits a finished message op's terminal event for
// the replaying request's endpoint, projected from the op (DES-O0API §6.3):
// a request whose original waiter missed the event still gets one. The
// projection carries no will_start, will_not_start_reason, interrupt or
// original error message.
func (s *Server) emitMessageOpProjection(operation string, op opjournal.Op) {
	wire := conversationOpWire(op)
	arm, reason := op.Outcome.Effective()
	if arm == opjournal.ArmSucceeded {
		queued := op.Outcome.Result.Via == legacyop.ViaLegacyQueued
		if operation == RequestOperationSessionSubmit {
			s.emitAsyncResult(events.RequestResultSessionSubmit, op.Conversation, SessionSubmitSucceededPayload{
				RequestID: op.ID,
				SessionID: op.Conversation,
				Queued:    queued,
				Intent:    string(submitIntentForWhen(op.Policy.Message.When)),
				Op:        wire,
			})
			return
		}
		s.emitAsyncResult(events.RequestResultSessionMessage, op.Conversation, SessionMessageSucceededPayload{
			RequestID: op.ID,
			SessionID: op.Conversation,
			Queued:    queued,
			Op:        wire,
		})
		return
	}
	s.emitRequestFailedOp(op.ID, operation, messageReplayErrorCode(operation, reason), replayedFailureMessage(op.ID, arm, reason), wire)
}

// replayedFailureMessage is the error message of a replayed failure: the op
// and its stored outcome; the original error text is not kept.
func replayedFailureMessage(id string, arm opjournal.Arm, reason string) string {
	message := fmt.Sprintf("replayed: operation %s already closed %s", id, arm)
	if reason != "" {
		message += "{" + reason + "}"
	}
	return message
}

// messageReplayErrorCode is the request.failed error code a replay projects
// from a failed or skipped message op's reason (DES-O0API §6.3).
func messageReplayErrorCode(operation, reason string) string {
	switch reason {
	case opjournal.ReasonUnsupported:
		return turnInterruptUnsupportedCode
	case opjournal.ReasonConversationClosed, opjournal.ReasonSessionGone:
		return "resolve_failed"
	case opjournal.ReasonDeadlineInFlight:
		return "timeout"
	}
	if operation == RequestOperationSessionSubmit {
		return "submit_failed"
	}
	return "message_failed"
}

// releaseOnPanic leaves adm's op open and releases its in-flight entry when
// the handler panics before handing the admission to its goroutine, so a
// same-key retry adopts the op instead of answering busy forever. It
// re-panics. handedOff reports whether the goroutine owns the admission.
func releaseOnPanic(adm *legacyop.Admission, handedOff *bool) {
	if *handedOff {
		return
	}
	if p := recover(); p != nil {
		adm.Finish(legacyop.Result{Outcome: opjournal.Outcome{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonOther}, LeaveOpen: true})
		panic(p)
	}
}

// opsDegradeInterval latches ops.recording_degraded per (gate, surface,
// stage, reason), the recorder's log interval.
const opsDegradeInterval = legacyop.DegradeLogInterval

// opsDegradeDetailMax bounds the event's detail.
const opsDegradeDetailMax = 512

// OpsDegradeReporter turns operation-journal recording degrades into
// ops.recording_degraded city events (DES-O0API D13): at most one event per
// (gate, surface, stage, reason) per legacyop.DegradeLogInterval, so a store
// outage emits once per interval instead of once per request. The API's
// recorders and the controller's op sweeper report through one each. It is
// safe for concurrent use.
type OpsDegradeReporter struct {
	recorder func() events.Recorder
	clk      clock.Clock

	mu   sync.Mutex
	last map[opsDegradeKey]time.Time
}

// opsDegradeKey is the latch key of one degrade class.
type opsDegradeKey struct {
	gate, surface, stage, reason string
}

// NewOpsDegradeReporter returns a reporter that records to the recorder
// recorder returns at each report (nil there drops the event) and dates its
// latch with clk.
func NewOpsDegradeReporter(recorder func() events.Recorder, clk clock.Clock) *OpsDegradeReporter {
	return &OpsDegradeReporter{recorder: recorder, clk: clk, last: map[opsDegradeKey]time.Time{}}
}

// Report records one ops.recording_degraded event for d under gate (a
// rollout key: ops.content, ops.control or ops.lifecycle), unless the latch
// for its class is still closed. A degrade under a mode that does not record
// (off) is not a degrade and is ignored.
func (r *OpsDegradeReporter) Report(gateKey string, d legacyop.Degrade) {
	if r == nil || (d.Mode != gate.Auto && d.Mode != gate.Require) {
		return
	}
	key := opsDegradeKey{gate: gateKey, surface: d.Surface, stage: d.Stage, reason: d.Reason}
	now := r.clk.Now()
	r.mu.Lock()
	if last, seen := r.last[key]; seen && now.Before(last.Add(opsDegradeInterval)) {
		r.mu.Unlock()
		return
	}
	r.last[key] = now
	r.mu.Unlock()
	rec := r.recorder()
	if rec == nil {
		return
	}
	detail := ""
	if d.Err != nil {
		detail = truncateUTF8(d.Err.Error(), opsDegradeDetailMax)
	}
	payload, err := json.Marshal(events.OpsRecordingDegradedPayload{
		Gate:    gateKey,
		Mode:    string(d.Mode),
		Surface: d.Surface,
		Stage:   d.Stage,
		Reason:  d.Reason,
		Detail:  detail,
	})
	if err != nil {
		log.Printf("api: marshal %s: %v", events.OpsRecordingDegraded, err)
		return
	}
	rec.Record(events.Event{
		Type:    events.OpsRecordingDegraded,
		Actor:   d.Surface,
		Subject: gateKey,
		Message: fmt.Sprintf("op recording degraded: gate=%s mode=%s surface=%s stage=%s reason=%s", gateKey, d.Mode, d.Surface, d.Stage, d.Reason),
		Payload: payload,
	})
}

// truncateUTF8 cuts s to at most n bytes without splitting a rune.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
