package api

// The API's O0 shadow ops on the content verbs, POST .../session/{id}/submit
// and .../messages (engdocs/design/des-o0-api-shadow-ops.md §6; DES-O0API
// below).
//
// Under the ops.content rollout gate each request is recorded as a message
// op before the legacy delivery runs and closed from the legacy result after
// it, through the legacyop front door. Off records nothing and answers as
// before, with two additions every mode shares, which ops.content does not
// gate: an interrupt_now submit's result carries the turn fence's
// "interrupt" report (DES-T3I §14 item 2), and a terminal event goes through
// the city's shared op state (below), which keeps one the event log refuses
// and answers a same-key retry of its request 409 in_flight, from the LRU
// too, until it is recorded. Auto records where the op store can and
// lets the request run unrecorded where it cannot. Require refuses a request
// it cannot record; the one effect that can precede the refusal is the
// materialization of a configured named session, which require needs to name
// the op's conversation (§6.4). The existing Idempotency-Key LRU stays in
// front, unchanged but for that 409; behind it the in-flight registry and
// the op row are the dedupe authority, so a retry finds its request again
// across a restart, across /submit and /messages, and across alias and bead
// id. The delivered text is the legacy path's, unchanged: no op id, key or
// marker reaches a runtime (D15, I11).
//
// The in-flight registry is per city per process (cityOps), not per Server:
// the supervisor builds a new Server for a city whenever the city's State
// changes, while the old Server's deliveries may still run, and adoption
// reads "not in the registry" as "its API died" (D9, I3). Such a delivery
// reports to the city's live Server, the one the supervisor serves the city
// with: its terminal event goes to that Server's event log and its op closes
// through that Server's store, since the city's restart may have closed the
// old State's. A close that keeps failing is retried until the op closes,
// its finish_by passes or the process shuts down, and its in-flight entry is
// held until the op closes or its finish_by passes: its message may have
// landed, so a same-key retry must not adopt and deliver it again. Past
// finish_by a same-key retry closes the op failed{deadline_in_flight}, as the
// controller's sweeper does, and replays it; it never adopts it. A recorded
// delivery's own timeout (sessionMessageAsyncTimeout,
// sessionSubmitAsyncTimeout) ends inside its op's finish_by, so the op
// closes from the delivery's result, or from that timeout, first.
//
// The supervisor builds a restarted city's new Server only when a request
// for the city arrives, so a delivery can finish after the restart closed
// the old State's event log while no new Server is live. A terminal event
// the event log refuses is kept, per city, and recorded in order the next
// time a log takes it: when the supervisor builds the city's next Server,
// before the city's next /submit or /messages terminal event, and before a
// same-key retry of its request is answered, from the LRU, the in-flight
// registry or the op row. While it cannot be, that retry is answered 409
// in_flight instead of a 202 with no event after its cursor, as is a replay
// whose re-emitted event the log refuses. When the log takes a kept event of
// a replay's request, endpoint and type, that event lands after the replay's
// cursor and stands for its re-emitted one, which is not recorded too. Kept
// events are bounded (maxKeptEvents) and live only in the process: one
// dropped at the bound, or still kept at shutdown, is logged.

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

// messageSentinels maps the message paths' legacy errors onto op outcomes
// (DES-O0API §6.3), the API's and the CLI's alike (MessageOpResult). A
// session whose row or runtime is gone is session_gone. The delivery
// timeouts of a recorded /messages and of a recorded /submit are reported
// as context.DeadlineExceeded.
var messageSentinels = legacyop.Sentinels{
	Unsupported:         []error{session.ErrTurnInterruptUnsupported},
	ConversationClosed:  []error{session.ErrSessionClosed},
	SessionGone:         []error{session.ErrSessionNotFound, runtime.ErrSessionNotFound},
	ProviderUnavailable: []error{runtime.ErrRuntimeUnavailable},
	DeliveryTimeout:     []error{context.DeadlineExceeded, context.Canceled},
}

// messageTimeoutError is a recorded /messages' delivery timeout: the legacy
// event text, classified as context.DeadlineExceeded for the op outcome.
type messageTimeoutError struct{}

// Error is the legacy timeout text.
func (messageTimeoutError) Error() string {
	return fmt.Sprintf("session.message timed out after %s", sessionMessageAsyncTimeout)
}

// Unwrap classifies the timeout as a deadline.
func (messageTimeoutError) Unwrap() error { return context.DeadlineExceeded }

// submitTimeoutError is a recorded /submit's delivery timeout, classified as
// context.DeadlineExceeded for the op outcome.
type submitTimeoutError struct{}

// Error names the bound the delivery outlived.
func (submitTimeoutError) Error() string {
	return fmt.Sprintf("session.submit timed out after %s", sessionSubmitAsyncTimeout)
}

// Unwrap classifies the timeout as a deadline.
func (submitTimeoutError) Unwrap() error { return context.DeadlineExceeded }

// opTargetMovedResult counts a recorded message whose target re-resolved to
// another session between the 202 and the delivery (DES-O0API §6.1).
const opTargetMovedResult = "op_target_moved"

// maxKeptEvents bounds the terminal events a city keeps for an event log that
// refuses them; past it the oldest is dropped, and logged.
const maxKeptEvents = 1024

// keptEvent is a terminal event an event log refused, kept to record later:
// requestID's, for operation's endpoint.
type keptEvent struct {
	requestID string
	operation string
	event     events.Event
}

// cityOps is the operation-journal state one process shares across every
// Server it builds for one city: the in-flight registry, which must outlive
// a Server whose deliveries are still running (I3); the degrade reporter,
// whose latch is per city per process (§5.5); the city's live Server, the
// one the supervisor serves the city with, through which every Server's
// deliveries report and close; the terminal events its event log refused,
// kept to record later; and the done channel that ends the city's background
// close retries when the process shuts down. It is safe for concurrent use.
type cityOps struct {
	inflight *legacyop.Inflight
	degraded *OpsDegradeReporter
	done     chan struct{}
	stopOnce sync.Once
	// logf writes the kept events' log lines; tests capture them.
	logf func(format string, args ...any)

	// emitMu serializes the city's terminal events, so the kept ones are
	// recorded in order and before any later one, and guards kept. It is
	// held across the event log's Record in every ops.content mode, off
	// included, so a slow log, such as an exec provider's script forked per
	// event, holds back the city's other terminal events, and the answer to
	// a same-key retry the LRU, the in-flight registry or the stored op
	// gives, until that Record returns. It is taken before mu.
	emitMu sync.Mutex
	kept   []keptEvent

	mu   sync.Mutex
	live *Server
}

// newCityOps returns a city's empty shared op state, with no live Server.
func newCityOps() *cityOps {
	c := &cityOps{inflight: legacyop.NewInflight(), done: make(chan struct{}), logf: log.Printf}
	c.degraded = NewOpsDegradeReporter(func() events.Recorder { return c.eventLog(nil) }, clock.Real{})
	return c
}

// publish makes s the city's live Server: the supervisor serves the city
// with it now.
func (c *cityOps) publish(s *Server) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.live = s
}

// retire leaves the city with no live Server when s is the live one: the
// supervisor replaced or dropped it.
func (c *cityOps) retire(s *Server) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.live == s {
		c.live = nil
	}
}

// liveServer is the city's live Server, or nil.
func (c *cityOps) liveServer() *Server {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.live
}

// eventLog is the event log the city's events go to: the live Server's, or
// from's when the city has none. It is nil when there is no such Server or
// its State has no event provider.
func (c *cityOps) eventLog(from *Server) events.Recorder {
	srv := c.liveServer()
	if srv == nil {
		srv = from
	}
	if srv == nil {
		return nil
	}
	ep := srv.state.EventProvider()
	if ep == nil {
		return nil
	}
	return ep
}

// recordAcked records e to rec and returns rec's refusal: an AckRecorder's
// error, such as a closed log's. A recorder that cannot acknowledge (an exec
// script) is taken at its word: recording the event to it again could only
// duplicate it.
func recordAcked(rec events.Recorder, e events.Event) error {
	if ack, ok := rec.(events.AckRecorder); ok {
		return ack.RecordAck(e)
	}
	rec.Record(e)
	return nil
}

// emit records requestID's terminal event e for operation's endpoint to the
// city's event log (from's, when the city has no live Server), after the
// events kept before it. An event the log refuses is kept, to be recorded
// when the supervisor builds the city's next Server, before the city's next
// emit, before a same-key retry of its request is answered, or when its
// request's outcome is looked up (lookupRequestOutcome).
func (c *cityOps) emit(from *Server, requestID, operation string, e events.Event) {
	c.emitMu.Lock()
	defer c.emitMu.Unlock()
	c.emitLocked(from, requestID, operation, e)
}

// emitReplay records a stored replay's terminal event e for requestID on
// operation's endpoint as emit does, and reports whether one of requestID's
// terminal events is still kept, the event log refusing it: the replay is
// busy. While one of them stays kept after the flush, e is neither recorded
// nor kept. A kept event the log takes now that matches e's request,
// endpoint and type, the first request's or an earlier replay's, lands after
// the replay's cursor and is its event, so e is not recorded too. One for the
// other endpoint does not stand in for e, since a waiter on this one would
// never match it, nor does one of another type: the executor's event of an
// op whose close kept failing, closed since with another outcome
// (engdocs/design/async-request-result.md, request_id rule 4).
func (c *cityOps) emitReplay(from *Server, requestID, operation string, e events.Event) bool {
	c.emitMu.Lock()
	defer c.emitMu.Unlock()
	keptAsE := slices.ContainsFunc(c.kept, func(k keptEvent) bool {
		return k.requestID == requestID && k.operation == operation && k.event.Type == e.Type
	})
	c.flushLocked(from)
	if c.keepsLocked(requestID) {
		return true
	}
	if !keptAsE {
		c.emitLocked(from, requestID, operation, e)
	}
	return c.keepsLocked(requestID)
}

// emitLocked is emit with c.emitMu held.
func (c *cityOps) emitLocked(from *Server, requestID, operation string, e events.Event) {
	rec := c.eventLog(from)
	if rec == nil {
		c.logf("api: no event provider for %s result %s", e.Type, requestID)
		return
	}
	err := c.recordKeptLocked(rec)
	if err == nil {
		err = recordAcked(rec, e)
	}
	if err == nil {
		return
	}
	c.logf("api: keeping the %s result %s, which the event log refused: %v", e.Type, requestID, err)
	if len(c.kept) == maxKeptEvents {
		c.logf("api: dropping the kept %s result %s: %d results are kept", c.kept[0].event.Type, c.kept[0].requestID, maxKeptEvents)
		c.kept = slices.Delete(c.kept, 0, 1)
	}
	c.kept = append(c.kept, keptEvent{requestID: requestID, operation: operation, event: e})
}

// recordKeptLocked records the kept events to rec, oldest first, and returns
// the first refusal: the refused event and those after it stay kept. c.emitMu
// must be held.
func (c *cityOps) recordKeptLocked(rec events.Recorder) error {
	for len(c.kept) > 0 {
		k := c.kept[0]
		if err := recordAcked(rec, k.event); err != nil {
			return err
		}
		c.logf("api: recorded the kept %s result %s", k.event.Type, k.requestID)
		c.kept = slices.Delete(c.kept, 0, 1)
	}
	return nil
}

// flushLocked records the kept events to the city's event log (from's, when
// the city has no live Server), leaving kept those it refuses. c.emitMu must
// be held.
func (c *cityOps) flushLocked(from *Server) {
	if len(c.kept) == 0 {
		return
	}
	rec := c.eventLog(from)
	if rec == nil {
		return
	}
	if err := c.recordKeptLocked(rec); err != nil {
		c.logf("api: the event log refused %d kept results: %v", len(c.kept), err)
	}
}

// flush records the kept events as flushLocked does.
func (c *cityOps) flush(from *Server) {
	c.emitMu.Lock()
	defer c.emitMu.Unlock()
	c.flushLocked(from)
}

// flushKeeps records the kept events as flush does and reports whether
// requestID's terminal event is still kept: the event log refused it.
func (c *cityOps) flushKeeps(from *Server, requestID string) bool {
	c.emitMu.Lock()
	defer c.emitMu.Unlock()
	c.flushLocked(from)
	return c.keepsLocked(requestID)
}

// keepsLocked reports whether one of requestID's terminal events is kept.
// c.emitMu must be held.
func (c *cityOps) keepsLocked(requestID string) bool {
	return slices.ContainsFunc(c.kept, func(k keptEvent) bool { return k.requestID == requestID })
}

// shutdown ends the city's background close retries: the process is shutting
// down. An op whose close was still being retried stays open, its in-flight
// entry held until its finish_by; past it the sweeper, or a same-key retry,
// closes the op expired. The kept terminal events are recorded if the city's
// event log takes them now; the rest are lost with the process, and logged.
// It is idempotent.
func (c *cityOps) shutdown() {
	c.stopOnce.Do(func() {
		close(c.done)
		c.emitMu.Lock()
		defer c.emitMu.Unlock()
		c.flushLocked(nil)
		for _, k := range c.kept {
			c.logf("api: the process is shutting down: the kept %s result %s is lost", k.event.Type, k.requestID)
		}
		c.kept = nil
	})
}

// serverOps is one Server's operation-journal state: a recorder per ops
// gate over its city's shared state (content for /submit and /messages,
// control for /stop and /respond, lifecycle for /wake, /suspend, /kill,
// /close and /reset; session_control_ops.go) and the lazily opened op store.
type serverOps struct {
	city      *cityOps
	content   *legacyop.Recorder
	control   *legacyop.Recorder
	lifecycle *legacyop.Recorder
	clk       clock.Clock
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

	mu   sync.Mutex
	st   *opjournal.Store
	over beads.Store
}

// newServerOps builds s's op state over its boot-latched gates and city, the
// city's shared state. It runs once at Server construction, after the gates
// are latched. The supervisor publishes the Server it serves a city with;
// a nil city gives s a city of its own, with s its live Server.
func newServerOps(s *Server, city *cityOps) *serverOps {
	if city == nil {
		city = newCityOps()
		city.publish(s)
	}
	o := &serverOps{
		city: city,
		clk:  clock.Real{},
		openStore: func(st beads.SessionStore, clk clock.Clock) (*opjournal.Store, error) {
			return opjournal.NewStore(st, clk)
		},
		logf:     log.Printf,
		recordOp: telemetry.RecordOpRecord,
	}
	o.content = o.newRecorder(s, rollout.KeyOpsContent, s.bootFlags.OpsContent())
	o.control = o.newRecorder(s, rollout.KeyOpsControl, s.bootFlags.OpsControl())
	o.lifecycle = o.newRecorder(s, rollout.KeyOpsLifecycle, s.bootFlags.OpsLifecycle())
	return o
}

// newRecorder is the API's recorder for the ops gate gateKey under mode: it
// records through s's op store and its city's in-flight registry, and reports
// degrades and health to its city's reporter under gateKey.
func (o *serverOps) newRecorder(s *Server, gateKey string, mode gate.Mode) *legacyop.Recorder {
	city := o.city
	return &legacyop.Recorder{
		Mode:     mode,
		Executor: legacyop.ExecutorAPI,
		Open:     s.openOpStore,
		Inflight: city.inflight,
		Clock:    o.clk,
		Degraded: func(d legacyop.Degrade) { city.degraded.Report(gateKey, d) },
		Healthy:  func(stage string) { o.healthyGate(gateKey, stage) },
		Done:     city.done,
	}
}

// publishOps makes s its city's live Server; a Server with no op state has
// nothing to publish.
func (s *Server) publishOps() {
	if s.ops != nil {
		s.ops.city.publish(s)
	}
}

// retireOps leaves s's city with no live Server when s is the live one.
func (s *Server) retireOps() {
	if s.ops != nil {
		s.ops.city.retire(s)
	}
}

// flushOps records the terminal events s's city kept while its event log
// refused them: the supervisor just published s, whose event log may take
// them.
func (s *Server) flushOps() {
	if s.ops != nil {
		s.ops.city.flush(s)
	}
}

// liveServer is the Server s's deliveries report and close through: its
// city's live Server, or s itself when the city has none (the supervisor
// dropped it) or s has no op state.
func (s *Server) liveServer() *Server {
	if s.ops == nil {
		return s
	}
	if live := s.ops.city.liveServer(); live != nil {
		return live
	}
	return s
}

// openOpStore opens the op store over the live Server's sessions store: a
// replaced Server's background close retry closes through the store of the
// State the supervisor serves the city with now, not the one the city's
// restart closed.
func (s *Server) openOpStore() (*opjournal.Store, error) {
	live := s.liveServer()
	return live.ops.store(live.state.SessionsBeadStore())
}

// healthy tells the degrade reporter that stages of the content gate just
// worked, so the next failure there raises a fresh event (§5.5).
func (o *serverOps) healthy(stages ...string) {
	o.healthyGate(rollout.KeyOpsContent, stages...)
}

// healthyGate tells the degrade reporter that stages of the ops gate gateKey
// just worked, so the next failure there raises a fresh event (§5.5).
func (o *serverOps) healthyGate(gateKey string, stages ...string) {
	o.city.degraded.Healthy(gateKey, legacyop.SurfaceAPI, stages...)
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
		Policy:       MessageOpPolicy(req.intent),
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
	body, err := s.replayAnswer(adm, req.operation, cursor)
	return nil, body, err
}

// replayAnswer is the body a Replay admission answers operation's request
// with (DES-O0API §6.2): the running request's 202 while it runs, else the
// stored op's, whose terminal event is re-emitted for operation's endpoint.
func (s *Server) replayAnswer(adm *legacyop.Admission, operation, cursor string) (*asyncAcceptedBody, error) {
	if adm.Cached != nil {
		answer, ok := adm.Cached.(inflightAnswer)
		if !ok || answer.operation != operation {
			// The running request emits its terminal event for its own
			// endpoint only, so a waiter on another endpoint would never
			// match it: busy until it finishes, then a stored replay.
			return nil, requestInFlight()
		}
		// Its terminal event may be kept, refused by the event log: record
		// it before answering, and answer busy while it cannot be, so the
		// 202 has its event after its cursor.
		if s.ops.city.flushKeeps(s, answer.body.RequestID) {
			return nil, requestInFlight()
		}
		return &answer.body, nil
	}
	body := asyncAcceptedBody{
		Status:         "accepted",
		RequestID:      adm.Op.ID,
		EventCursor:    cursor,
		IdempotencyKey: adm.Key,
		Replayed:       true,
		Op:             conversationOpWire(adm.Op),
	}
	// The replay's re-emitted event is a terminal event too: busy while one
	// of the op's is kept, without keeping another, so the 202 has its event
	// after its cursor, and only one: a kept one of its type for this
	// endpoint, recorded now, is that event (cityOps.emitReplay).
	if s.emitMessageOpProjection(operation, adm.Op) {
		return nil, requestInFlight()
	}
	return &body, nil
}

// withMessageIdempotency is withIdempotency for /submit and /messages: a
// retry the LRU answers from its cached 202 is answered busy while that
// request's terminal event is kept, as replayAnswer answers one the LRU
// misses, so the 202 has its event after its cursor.
func (s *Server) withMessageIdempotency(path, key string, body any, accept func() (asyncAcceptedBody, error)) (asyncAcceptedBody, error) {
	cached := true
	v, err := withIdempotency(s.idem, path, key, body, func() (asyncAcceptedBody, error) {
		cached = false
		return accept()
	})
	if err != nil || !cached || s.ops == nil {
		return v, err
	}
	if s.ops.city.flushKeeps(s, v.RequestID) {
		return asyncAcceptedBody{}, requestInFlight()
	}
	return v, nil
}

// requestInFlight answers a same-key request busy (409 in_flight), as the
// Idempotency-Key LRU answers one whose first request is still running.
func requestInFlight() error {
	return apierr.IdempotencyInFlight.Msg("in_flight: request with this Idempotency-Key is already in progress")
}

// MessageOpPolicy is the message policy of a submit intent (DES-O0API §3.1);
// /messages is the default intent. gc session submit's local path records its
// op under the same policy, so a request the API recorded and its CLI
// fallback derive one op digest.
func MessageOpPolicy(intent session.SubmitIntent) opjournal.Policy {
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
		return requestInFlight()
	case errors.Is(err, opjournal.ErrKeyReuse):
		return apierr.IdempotencyMismatch.Msg("idempotency_mismatch: Idempotency-Key reused with a different request")
	case errors.Is(err, legacyop.ErrUnavailable):
		return apierr.OpJournalUnavailable.Msg("op_journal_unavailable: the operation journal cannot record this request; nothing was done: " + err.Error())
	case errors.Is(err, opjournal.ErrPayloadTooLarge):
		return apierr.PayloadTooLarge.Msg(fmt.Sprintf("payload_too_large: the request exceeds the operation journal's %d-byte limit", opjournal.MaxInlinePayload))
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

// MessageOpResult is the op result of a message delivery's legacy result,
// its error mapped by messageSentinels: a delivery whose interrupt the runtime
// accepted unfenced records that degradation. The API and the CLI's message
// paths all close a message op through it, so one op id closes alike on
// either surface.
func MessageOpResult(queued bool, err error, interrupt *session.TurnInterruptReport) legacyop.Result {
	res := messageSentinels.MessageResult(queued, err)
	if err == nil && interrupt != nil && interrupt.Degradation != "" {
		res.Outcome.Degradation = interrupt.Degradation
	}
	return res
}

// finishMessageOp closes adm from res, before the terminal event is emitted
// (I10), and returns the op that event carries: nil when the request was not
// recorded. A close that keeps failing retries in the background, holding the
// in-flight entry, and the event is emitted at once with the op still open,
// in every mode (DES-O0API §6.1 step 8): a client stops waiting long before
// the op's finish_by, and its retry without a key would deliver again.
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
	return s.currentOpWire(adm.Op)
}

// currentOpWire re-reads op for the wire through the live Server's op store,
// falling back to the copy held when the read fails.
func (s *Server) currentOpWire(op opjournal.Op) *ConversationOp {
	if st := s.liveServer().ops.cached(); st != nil {
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
// a request whose original waiter missed the event still gets one. It
// reports whether one of the op's terminal events is still kept, the event
// log refusing it, as cityOps.emitReplay does. The projection carries no
// will_start, will_not_start_reason, interrupt or original error message; its
// message, the error_message of a failure, says what it replays and that the
// retry's own options took no effect (§3.1).
func (s *Server) emitMessageOpProjection(operation string, op opjournal.Op) bool {
	wire := conversationOpWire(op)
	arm, reason := op.Outcome.Effective()
	message := replayMessage(op.ID, arm, reason)
	if arm == opjournal.ArmSucceeded {
		queued := op.Outcome.Result.Via == legacyop.ViaLegacyQueued
		if operation == RequestOperationSessionSubmit {
			return s.emitReplayEvent(operation, events.RequestResultSessionSubmit, op.Conversation, message, SessionSubmitSucceededPayload{
				RequestID: op.ID,
				SessionID: op.Conversation,
				Queued:    queued,
				Intent:    string(submitIntentForWhen(op.Policy.Message.When)),
				Op:        wire,
			})
		}
		return s.emitReplayEvent(operation, events.RequestResultSessionMessage, op.Conversation, message, SessionMessageSucceededPayload{
			RequestID: op.ID,
			SessionID: op.Conversation,
			Queued:    queued,
			Op:        wire,
		})
	}
	return s.emitReplayEvent(operation, events.RequestFailed, "", message, RequestFailedPayload{
		RequestID:    op.ID,
		Operation:    operation,
		ErrorCode:    messageReplayErrorCode(operation, reason),
		ErrorMessage: message,
		Op:           wire,
	})
}

// emitReplayEvent records a replay's terminal event for operation's endpoint
// through its city's op state (cityOps.emitReplay), with the replay's message
// on the event, and reports whether one of its request's terminal events is
// still kept.
func (s *Server) emitReplayEvent(operation, eventType, subject, message string, payload events.Payload) bool {
	requestID := requestIDFromPayload(payload)
	e, err := typedEvent(eventType, subject, payload)
	if err != nil {
		s.ops.logf("api: %v", err)
		return s.ops.city.flushKeeps(s, requestID)
	}
	e.Message = message
	return s.ops.city.emitReplay(s, requestID, operation, e)
}

// replayOptionsNote ends a replay's message (DES-O0API §3.1): resume and the
// request's other execution options are outside the op and its digest, so a
// same-key retry that changes one gets the first request's outcome without it.
const replayOptionsNote = "a replay runs nothing, so this request's own options, such as resume, took no effect: wake the session to start it, or use a new Idempotency-Key to send again"

// replayMessage is a replay's message: the op and its stored outcome (the
// original error text is not kept), and that the retry's options took no
// effect.
func replayMessage(id string, arm opjournal.Arm, reason string) string {
	message := fmt.Sprintf("replayed: operation %s already closed %s", id, arm)
	if reason != "" {
		message += "{" + reason + "}"
	}
	return message + "; " + replayOptionsNote
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
// and at most one per legacyop.DegradeLogInterval, a failure the interval
// holds back raising its event with the class's first failure once the
// interval has passed. So a store outage emits once instead of once per
// request, even one that begins inside the interval after a short blip. A
// class is healthy until it fails, and again once its stage reports success
// (Healthy). The API reports through one per city per process, the
// controller's op sweeper through its own. It is safe for concurrent use.
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

// opsDegradeLatch is one degrade class's state: when it last raised an event,
// whether it has failed since its stage last reported success, and whether
// the interval held back the event of that failure (owed), which the class's
// first failure once the interval has passed raises.
type opsDegradeLatch struct {
	lastEvent time.Time
	degraded  bool
	owed      bool
}

// NewOpsDegradeReporter returns a reporter that records to the recorder
// recorder returns at each report and dates its latch with clk. A report
// made while recorder returns nil, or whose event the recorder refuses, is
// dropped without latching its class.
func NewOpsDegradeReporter(recorder func() events.Recorder, clk clock.Clock) *OpsDegradeReporter {
	return &OpsDegradeReporter{recorder: recorder, clk: clk, classes: map[opsDegradeKey]opsDegradeLatch{}}
}

// Report records one ops.recording_degraded event for d under gate (a
// rollout key: ops.content, ops.control or ops.lifecycle) when d's class was
// healthy, or owes the event of a failure the interval held back, and its
// last event is at least opsDegradeInterval old; either way the class is
// degraded until its stage reports success. A degrade under a
// mode that does not record (off) is not a degrade and is ignored. One with
// no recorder to record to, or whose event the recorder refuses (an
// AckRecorder's error, such as a closed log's), leaves its class as it was,
// so the class's next failure raises the event again: a degrade event is not
// kept, as a terminal event is.
func (r *OpsDegradeReporter) Report(gateKey string, d legacyop.Degrade) {
	if r == nil || (d.Mode != gate.Auto && d.Mode != gate.Require) {
		return
	}
	rec := r.recorder()
	if rec == nil {
		return
	}
	key := opsDegradeKey{gate: gateKey, surface: d.Surface, stage: d.Stage, reason: d.Reason}
	now := r.clk.Now()
	prior, seen, due := r.markDegraded(key, now)
	if !due {
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
		r.unmark(key, prior, seen, now)
		return
	}
	err = recordAcked(rec, events.Event{
		Type:    events.OpsRecordingDegraded,
		Actor:   d.Surface,
		Subject: gateKey,
		Message: fmt.Sprintf("op recording degraded: gate=%s mode=%s surface=%s stage=%s reason=%s", gateKey, d.Mode, d.Surface, d.Stage, d.Reason),
		Payload: payload,
	})
	if err != nil {
		log.Printf("api: the event log refused %s for gate=%s surface=%s stage=%s reason=%s: %v", events.OpsRecordingDegraded, gateKey, d.Surface, d.Stage, d.Reason, err)
		r.unmark(key, prior, seen, now)
	}
}

// markDegraded marks key's class degraded at now and reports whether this
// failure raises an event (due): the class was healthy or owes an event, and
// its last event is at least opsDegradeInterval old. A due failure dates the
// class's last event now; one only the interval holds back leaves the event
// owed. prior and seen are the class's state before the mark, for unmark.
func (r *OpsDegradeReporter) markDegraded(key opsDegradeKey, now time.Time) (prior opsDegradeLatch, seen, due bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	prior, seen = r.classes[key]
	news := !prior.degraded || prior.owed
	due = news && (!seen || !now.Before(prior.lastEvent.Add(opsDegradeInterval)))
	latch := prior
	latch.degraded = true
	latch.owed = news && !due
	if due {
		latch.lastEvent = now
	}
	r.classes[key] = latch
	return prior, seen, due
}

// unmark reverts the due mark made at now on key's class, whose event was not
// recorded, to its prior state, or forgets the class when it was not seen, so
// its next failure raises the event. A class whose last event is dated
// otherwise since is left as it is.
func (r *OpsDegradeReporter) unmark(key opsDegradeKey, prior opsDegradeLatch, seen bool, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.classes[key]
	if !ok || !cur.lastEvent.Equal(now) {
		return
	}
	if !seen {
		delete(r.classes, key)
		return
	}
	r.classes[key] = prior
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
			latch.degraded, latch.owed = false, false
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
