package api

// The API's O0 shadow ops on the content verbs, POST .../session/{id}/submit
// and .../messages (engdocs/design/des-o0-api-shadow-ops.md §6; DES-O0API
// below).
//
// Under the ops.content rollout gate each request is recorded as a message
// op before the legacy delivery runs and closed from the legacy result after
// it, through the legacyop front door. Off records nothing and answers as
// before, with one addition every mode shares: an interrupt_now submit's
// result carries the turn fence's "interrupt" report (DES-T3I §14 item 2),
// which ops.content does not gate. Auto records where the op store can and
// lets the request run unrecorded where it cannot. Require refuses a request
// it cannot record; the one effect that can precede the refusal is the
// materialization of a configured named session, which require needs to name
// the op's conversation (§6.4). The existing Idempotency-Key LRU stays in
// front, unchanged; behind it the in-flight registry and the op row are the
// dedupe authority, so a retry finds its request again across a restart,
// across /submit and /messages, and across alias and bead id. The delivered
// text is the legacy path's, unchanged: no op id, key or marker reaches a
// runtime (D15, I11).
//
// The in-flight registry is per city per process (cityOps), not per Server:
// the supervisor builds a new Server for a city whenever the city's State
// changes, while the old Server's deliveries may still run, and adoption
// reads "not in the registry" as "its API died" (D9, I3). For the same
// reason a replaced or dropped Server whose op close is still failing stops
// retrying but keeps the entry until the op's finish_by: its message may
// have landed, so a same-key retry must not adopt and deliver it again.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
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

// cityOps is the operation-journal state one process shares across every
// Server it builds for one city: the in-flight registry, which must outlive
// a Server whose deliveries are still running (I3), and the degrade
// reporter, whose latch is per process (§5.5). It is safe for concurrent use.
type cityOps struct {
	inflight *legacyop.Inflight
	degraded *OpsDegradeReporter

	mu       sync.Mutex
	recorder func() events.Recorder
}

// newCityOps returns a city's empty shared op state.
func newCityOps() *cityOps {
	c := &cityOps{inflight: legacyop.NewInflight()}
	c.degraded = NewOpsDegradeReporter(c.eventRecorder, clock.Real{})
	return c
}

// attach makes recorder, the newest Server's event recorder, the one the
// city's degrade events go to.
func (c *cityOps) attach(recorder func() events.Recorder) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recorder = recorder
}

// eventRecorder is the attached Server's event recorder, or nil.
func (c *cityOps) eventRecorder() events.Recorder {
	c.mu.Lock()
	recorder := c.recorder
	c.mu.Unlock()
	if recorder == nil {
		return nil
	}
	return recorder()
}

// serverOps is one Server's operation-journal state: the content recorder
// over its city's shared state, the lazily opened op store, and the done
// channel that ends the recorder's background close retries when the Server
// is dropped.
type serverOps struct {
	city    *cityOps
	content *legacyop.Recorder
	clk     clock.Clock
	// openStore opens the op store over a sessions-class store; tests
	// substitute it.
	openStore func(beads.SessionStore, clock.Clock) (*opjournal.Store, error)
	// logf writes the op path's log lines; tests capture them.
	logf func(format string, args ...any)
	// recordOp is telemetry.RecordOpRecord; tests capture it.
	recordOp func(ctx context.Context, kind, surface, stage, result, unrecordedReason string)
	// afterAdmit, when set, runs after a request is admitted and before its
	// delivery goroutine owns the admission; tests inject a handler panic
	// there.
	afterAdmit func()

	done     chan struct{}
	stopOnce sync.Once

	mu   sync.Mutex
	st   *opjournal.Store
	over beads.Store
}

// newServerOps builds s's op state over its boot-latched gates and city, the
// city's shared state (nil: a city of its own). It runs once at Server
// construction, after the gates are latched.
func newServerOps(s *Server, city *cityOps) *serverOps {
	if city == nil {
		city = newCityOps()
	}
	city.attach(func() events.Recorder { return s.state.EventProvider() })
	o := &serverOps{
		city: city,
		clk:  clock.Real{},
		openStore: func(st beads.SessionStore, clk clock.Clock) (*opjournal.Store, error) {
			return opjournal.NewStore(st, clk)
		},
		logf:     log.Printf,
		recordOp: telemetry.RecordOpRecord,
		done:     make(chan struct{}),
	}
	o.content = &legacyop.Recorder{
		Mode:     s.bootFlags.OpsContent(),
		Executor: legacyop.ExecutorAPI,
		Open:     func() (*opjournal.Store, error) { return o.store(s.state.SessionsBeadStore()) },
		Inflight: city.inflight,
		Clock:    o.clk,
		Degraded: func(d legacyop.Degrade) { city.degraded.Report(rollout.KeyOpsContent, d) },
		Done:     o.done,
	}
	return o
}

// shutdown ends the Server's background close retries: the supervisor
// dropped or replaced it. An op whose close was still being retried stays
// open, its in-flight entry held until its finish_by, then the sweeper's.
// It is idempotent and does nothing on nil.
func (o *serverOps) shutdown() {
	if o == nil {
		return
	}
	o.stopOnce.Do(func() { close(o.done) })
}

// healthy tells the degrade reporter that stages of the content gate just
// worked, so the next failure there raises a fresh event (§5.5).
func (o *serverOps) healthy(stages ...string) {
	o.city.degraded.Healthy(rollout.KeyOpsContent, legacyop.SurfaceAPI, stages...)
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

// inflightAnswer is what an admitted request leaves in its in-flight entry
// for a same-key request to replay while it runs: its 202 and the operation
// its terminal event names.
type inflightAnswer struct {
	operation string
	body      asyncAcceptedBody
}

// admitMessageOp admits req under the ops.content gate (DES-O0API §6.1 steps
// 3 and 4). It returns either an admission to execute or the body to answer
// a replay with (DES-O0API §6.2), or a refusal mapped to the wire. cursor is
// the event cursor captured before admission: a stored replay re-emits its
// terminal event after it.
func (s *Server) admitMessageOp(ctx context.Context, store beads.Store, req *messageOpRequest, cursor string) (*legacyop.Admission, *asyncAcceptedBody, error) {
	rec := s.ops.content
	oreq := legacyop.Request{
		Conversation: req.conv,
		Principal:    legacyop.PrincipalOperator,
		Actor:        opjournal.ActorOperator,
		Key:          req.key,
		Payload:      req.text,
		Kind:         opjournal.KindMessage,
		Policy:       messageOpPolicy(req.intent),
	}
	if req.conv == "" {
		if rec.Mode != gate.Require {
			return rec.Unrecorded(opjournal.KindMessage, req.key, legacyop.ReasonUnmaterializedTarget), nil, nil
		}
		// Require: refuse what cannot be recorded whatever the conversation
		// (a bad key, a payload over the limit, a store that does not open)
		// before materializing anything; then materialize the named session,
		// as /wake does, so the op names its conversation (DES-O0API §6.4).
		if err := rec.Check(oreq); err != nil {
			return nil, nil, opAdmitError(err, req.key)
		}
		id, err := s.resolveSessionIDMaterializingNamedWithContext(ctx, store, req.target)
		if err != nil {
			return nil, nil, humaResolveError(err)
		}
		req.conv, oreq.Conversation = id, id
	}
	adm, err := rec.Admit(oreq)
	if err != nil {
		return nil, nil, opAdmitError(err, req.key)
	}
	if adm.Recorded {
		s.ops.healthy(legacyop.StageOpen, legacyop.StagePropose)
		if adm.Adopted {
			s.ops.healthy(legacyop.StageAdopt)
		}
	}
	if adm.Decision != legacyop.Replay {
		return adm, nil, nil
	}
	if adm.Cached != nil {
		answer, ok := adm.Cached.(inflightAnswer)
		if !ok || answer.operation != req.operation {
			// The running request emits its terminal event for its own
			// endpoint only, so a waiter on another endpoint would never
			// match it: busy until it finishes, then a stored replay.
			return nil, nil, apierr.IdempotencyInFlight.Msg("in_flight: request with this Idempotency-Key is already in progress")
		}
		return nil, &answer.body, nil
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
		s.ops.healthy(legacyop.StageClose)
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

// noteTargetMoved logs and counts a recorded message whose target
// re-resolved to another session than the one its op names; the op still
// closes from the legacy result (DES-O0API §6.1).
func (s *Server) noteTargetMoved(adm *legacyop.Admission, delivered string) {
	if adm == nil || !adm.Recorded || delivered == adm.Op.Conversation {
		return
	}
	s.ops.logf("api: op %s for session %s was delivered to %s: the target moved after the 202", adm.Op.ID, adm.Op.Conversation, delivered)
	s.ops.recordOp(context.Background(), string(opjournal.KindMessage), legacyop.SurfaceAPI, legacyop.StageClose, opTargetMovedResult, "")
}

// runAfterAdmit runs the test hook between admission and the delivery
// goroutine's hand-off.
func (o *serverOps) runAfterAdmit() {
	if o.afterAdmit != nil {
		o.afterAdmit()
	}
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
// from a failed or skipped message op's reason (DES-O0API §6.3). The reason
// keeps the failure's class, not the step that failed, so a replay's code
// can differ from the original's: a session closed or gone during the
// delivery replays as resolve_failed, a deadline or cancellation inside
// /submit as timeout (engdocs/design/async-request-result.md, request_id
// rule 7).
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

// opsDegradeInterval is the least time between two ops.recording_degraded
// events of one (gate, surface, stage, reason), the recorder's log interval.
const opsDegradeInterval = legacyop.DegradeLogInterval

// opsDegradeDetailMax bounds the event's detail.
const opsDegradeDetailMax = 512

// OpsDegradeReporter turns operation-journal recording degrades into
// ops.recording_degraded city events (DES-O0API D13, §5.5): one event on the
// first failure of a (gate, surface, stage, reason) after a healthy period,
// and at most one per legacyop.DegradeLogInterval, so a store outage emits
// once instead of once per request. A class is healthy until it fails, and
// again once its stage reports success (Healthy). The API reports through one
// per city per process, the controller's op sweeper through its own. It is
// safe for concurrent use.
type OpsDegradeReporter struct {
	recorder func() events.Recorder
	clk      clock.Clock

	mu      sync.Mutex
	classes map[opsDegradeKey]opsDegradeLatch
}

// opsDegradeKey is the latch key of one degrade class.
type opsDegradeKey struct {
	gate, surface, stage, reason string
}

// opsDegradeLatch is one degrade class's state: when it last raised an event
// and whether it has failed since its stage last reported success.
type opsDegradeLatch struct {
	lastEvent time.Time
	degraded  bool
}

// NewOpsDegradeReporter returns a reporter that records to the recorder
// recorder returns at each report (nil there drops the event) and dates its
// latch with clk.
func NewOpsDegradeReporter(recorder func() events.Recorder, clk clock.Clock) *OpsDegradeReporter {
	return &OpsDegradeReporter{recorder: recorder, clk: clk, classes: map[opsDegradeKey]opsDegradeLatch{}}
}

// Report records one ops.recording_degraded event for d under gate (a
// rollout key: ops.content, ops.control or ops.lifecycle) when d's class was
// healthy and its last event is at least opsDegradeInterval old; either way
// the class is degraded until its stage reports success. A degrade under a
// mode that does not record (off) is not a degrade and is ignored.
func (r *OpsDegradeReporter) Report(gateKey string, d legacyop.Degrade) {
	if r == nil || (d.Mode != gate.Auto && d.Mode != gate.Require) {
		return
	}
	key := opsDegradeKey{gate: gateKey, surface: d.Surface, stage: d.Stage, reason: d.Reason}
	now := r.clk.Now()
	r.mu.Lock()
	latch, seen := r.classes[key]
	due := !latch.degraded && (!seen || !now.Before(latch.lastEvent.Add(opsDegradeInterval)))
	latch.degraded = true
	if due {
		latch.lastEvent = now
	}
	r.classes[key] = latch
	r.mu.Unlock()
	if !due {
		return
	}
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

// Healthy marks every degrade class of gate and surface at stages healthy:
// those stages just worked (a recorded admission for open and propose, an
// adoption for adopt, a close, a clean sweep pass), so their next failure is
// news again, subject to the interval.
func (r *OpsDegradeReporter) Healthy(gateKey, surface string, stages ...string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, latch := range r.classes {
		if key.gate == gateKey && key.surface == surface && latch.degraded && slices.Contains(stages, key.stage) {
			latch.degraded = false
			r.classes[key] = latch
		}
	}
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
