package storebinding

// The production class wrapper stack.
//
// Every resolved class front door is wrapped once, here, with the behavior the
// running system needs around persistence: cache, observation, and the
// maintenance/error discipline. The stack is provider-neutral by construction —
// it speaks only the closed class contracts, so the implicit legacy binding,
// the built-in SQLite binding and an out-of-tree provider's binding all execute
// the same wrapper code.
//
// WHAT A WRAPPER MAY NOT DO. It may not change what an operation returns. Inner
// errors are passed back VERBATIM, never re-wrapped: a caller matching
// beads.ErrNotFound, ErrNudgeSessionFenceMismatch, or any other sentinel must
// see exactly the error the binding produced, and an error string is part of
// observable behavior. Only errors the WRAPPER itself originates — a refused
// construction, a maintenance failure — carry wrapper context. A wrapper also
// never retries, never falls back to another store, and never re-resolves a
// provider: a failing binding fails, visibly, at the call that failed.
//
// WHY EMBEDDING. Each wrapper embeds its contract interface, so every operation
// it does not override is forwarded unchanged and every capability the binding
// declares survives wrapping by construction. Overrides are the exception, and
// the cache census in cache_graph_test.go proves the exceptional set is exactly
// the set the cache requires.
//
// Construction fails LOUDLY. A nil inner front door or a class the binding
// declares unavailable is refused at Wrap time rather than at the first call,
// because a typed-nil front door reaching a consumer is a nil dereference in
// production and a capability quietly lost at boot is worse.
//
// WORK IS NOT WRAPPED HERE. Five of the six classes reach their storage through
// a closed contract interface, which is what a wrapper can decorate. Work
// reaches it through [WorkTopology], an immutable VALUE holding one raw
// beads.Store per scope — there is no class contract to decorate, so wrapping
// Work means wrapping the bead engine itself, which is a different (and
// provider-shaped) question. Work's lifecycle and status are covered: the
// binding serving it is owned and closed by [BindingLifecycle] and described by
// [BindingLifecycle.Health] exactly like every other binding. Only its
// per-operation events and cache are absent.
//
// BEAD POLICY IS NOT APPLIED HERE, AND THAT IS THE POINT. The deployed storage
// policy — the per-bead storage class chosen at create, and the read-tier
// expansion on list and ready queries — decorates the canonical bead store
// BEFORE that store is projected into the class front doors, so it is already
// beneath every contract this file wraps. A second policy layer above the
// contract would apply it twice: an ephemeral bead would be re-classified on
// the way down and a read tier expanded onto an already-expanded query. The
// wrapper therefore forwards creates and queries VERBATIM, and a test pins
// that it does, because "we simply did not add it" is not a durable reason.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/session"
)

// ErrInvalidClassWrapping reports a wrapper that cannot be constructed over the
// supplied front door.
var ErrInvalidClassWrapping = errors.New("invalid storage class wrapping")

// ClassWrapping configures one class's production wrapper.
type ClassWrapping struct {
	// Binding names the binding the wrapped front door came out of. It appears
	// in every emitted event and in status.
	Binding BindingName
	// Capability is what the binding declares for this class. Wrapping a class
	// the binding says it does not serve is refused.
	Capability ClassCapability
	// Observer receives the class event stream. A nil observer disables
	// emission; it never disables the operation.
	Observer ClassObserver
	// CacheReads enables the Graph read cache. It is ignored by classes that
	// have no cache.
	CacheReads bool
}

func (w ClassWrapping) validate(class coordclass.Class) error {
	if err := validateIdentifier("wrapped binding name", string(w.Binding)); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrInvalidClassWrapping, class, err)
	}
	if !w.Capability.Available {
		return fmt.Errorf("%w: %s: %w", ErrInvalidClassWrapping, class, ErrMissingCapability)
	}
	return nil
}

func (w ClassWrapping) emitter(class coordclass.Class) classEmitter {
	return classEmitter{class: class, binding: w.Binding, observer: w.Observer}
}

func refuseWrapping(class coordclass.Class, reason string) error {
	return fmt.Errorf("%w: %s: %s", ErrInvalidClassWrapping, class, reason)
}

// WrapGraph puts the production stack in front of one Graph binding.
func WrapGraph(inner GraphStore, wrapping ClassWrapping) (GraphStore, error) {
	if err := wrapping.validate(coordclass.ClassGraph); err != nil {
		return nil, err
	}
	if isNilInterface(inner) {
		return nil, refuseWrapping(coordclass.ClassGraph, "no front door to wrap")
	}
	wrapped := &wrappedGraphStore{
		GraphStore: inner,
		emitter:    wrapping.emitter(coordclass.ClassGraph),
	}
	if wrapping.CacheReads {
		wrapped.cache = newGraphBeadCache()
	}
	return wrapped, nil
}

// wrappedGraphStore is the Graph class stack: cache, then observation.
type wrappedGraphStore struct {
	GraphStore
	emitter classEmitter
	cache   *graphBeadCache
}

// mutate runs one contract mutation inside the cache fence.
func (g *wrappedGraphStore) mutate(run func() error) error {
	if g.cache == nil {
		return run()
	}
	g.cache.enterMutation()
	defer g.cache.leaveMutation()
	return run()
}

func (g *wrappedGraphStore) write(operation string, run func() error) error {
	err := g.mutate(run)
	g.emitter.observe(ClassEventWrite, operation, err)
	return err
}

func (g *wrappedGraphStore) Get(id string) (beads.Bead, error) {
	if g.cache == nil {
		bead, err := g.GraphStore.Get(id)
		g.emitter.observe(ClassEventRead, "Get", err)
		return bead, err
	}
	if cached, found := g.cache.lookup(id); found {
		g.emitter.emit(ClassEventCache, "Get", ClassOutcomeHit)
		return cached, nil
	}
	g.emitter.emit(ClassEventCache, "Get", ClassOutcomeMiss)
	generation := g.cache.begin()
	bead, err := g.GraphStore.Get(id)
	g.emitter.observe(ClassEventRead, "Get", err)
	if err != nil {
		return bead, err
	}
	g.cache.install(id, bead, generation)
	return bead, nil
}

func (g *wrappedGraphStore) Ping() error {
	err := g.GraphStore.Ping()
	g.emitter.observe(ClassEventProbe, "Ping", err)
	return err
}

func (g *wrappedGraphStore) Create(bead beads.Bead) (beads.Bead, error) {
	var created beads.Bead
	err := g.write("Create", func() error {
		var err error
		created, err = g.GraphStore.Create(bead)
		return err
	})
	return created, err
}

func (g *wrappedGraphStore) CreateWithStorage(bead beads.Bead, storage beads.StorageClass) (beads.Bead, error) {
	var created beads.Bead
	err := g.write("CreateWithStorage", func() error {
		var err error
		created, err = g.GraphStore.CreateWithStorage(bead, storage)
		return err
	})
	return created, err
}

func (g *wrappedGraphStore) Update(id string, opts beads.UpdateOpts) error {
	return g.write("Update", func() error { return g.GraphStore.Update(id, opts) })
}

func (g *wrappedGraphStore) SetMetadata(id, key, value string) error {
	return g.write("SetMetadata", func() error { return g.GraphStore.SetMetadata(id, key, value) })
}

func (g *wrappedGraphStore) SetMetadataBatch(id string, values map[string]string) error {
	return g.write("SetMetadataBatch", func() error { return g.GraphStore.SetMetadataBatch(id, values) })
}

func (g *wrappedGraphStore) UpdateIfMatch(id string, revision int64, opts beads.UpdateOpts) error {
	return g.write("UpdateIfMatch", func() error { return g.GraphStore.UpdateIfMatch(id, revision, opts) })
}

func (g *wrappedGraphStore) CloseIfMatch(id string, revision int64) error {
	return g.write("CloseIfMatch", func() error { return g.GraphStore.CloseIfMatch(id, revision) })
}

func (g *wrappedGraphStore) DeleteIfMatch(id string, revision int64) error {
	return g.write("DeleteIfMatch", func() error { return g.GraphStore.DeleteIfMatch(id, revision) })
}

func (g *wrappedGraphStore) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	var swapped bool
	err := g.mutate(func() error {
		var err error
		swapped, err = g.GraphStore.CompareAndSetMetadataKey(id, key, expected, next)
		return err
	})
	g.emitter.observeClaim("CompareAndSetMetadataKey", swapped, err)
	return swapped, err
}

func (g *wrappedGraphStore) Close(id string) error {
	return g.write("Close", func() error { return g.GraphStore.Close(id) })
}

func (g *wrappedGraphStore) Reopen(id string) error {
	return g.write("Reopen", func() error { return g.GraphStore.Reopen(id) })
}

func (g *wrappedGraphStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	var closed int
	err := g.write("CloseAll", func() error {
		var err error
		closed, err = g.GraphStore.CloseAll(ids, metadata)
		return err
	})
	return closed, err
}

func (g *wrappedGraphStore) Delete(id string) error {
	return g.write("Delete", func() error { return g.GraphStore.Delete(id) })
}

func (g *wrappedGraphStore) Claim(id, assignee string) (beads.Bead, bool, error) {
	var (
		claimed  beads.Bead
		acquired bool
	)
	err := g.mutate(func() error {
		var err error
		claimed, acquired, err = g.GraphStore.Claim(id, assignee)
		return err
	})
	g.emitter.observeClaim("Claim", acquired, err)
	return claimed, acquired, err
}

func (g *wrappedGraphStore) ReleaseIfCurrent(id, assignee string) (bool, error) {
	var released bool
	err := g.mutate(func() error {
		var err error
		released, err = g.GraphStore.ReleaseIfCurrent(id, assignee)
		return err
	})
	g.emitter.observeClaim("ReleaseIfCurrent", released, err)
	return released, err
}

func (g *wrappedGraphStore) DepAdd(from, to, depType string) error {
	return g.write("DepAdd", func() error { return g.GraphStore.DepAdd(from, to, depType) })
}

func (g *wrappedGraphStore) DepRemove(from, to string) error {
	return g.write("DepRemove", func() error { return g.GraphStore.DepRemove(from, to) })
}

func (g *wrappedGraphStore) Tx(id string, run func(GraphTx) error) error {
	err := g.mutate(func() error { return g.GraphStore.Tx(id, run) })
	g.emitter.observe(ClassEventTransaction, "Tx", err)
	return err
}

func (g *wrappedGraphStore) ApplyGraphPlan(ctx context.Context, plan *beads.GraphApplyPlan) (*beads.GraphApplyResult, error) {
	var result *beads.GraphApplyResult
	err := g.mutate(func() error {
		var err error
		result, err = g.GraphStore.ApplyGraphPlan(ctx, plan)
		return err
	})
	g.emitter.observe(ClassEventGraphApply, "ApplyGraphPlan", err)
	return result, err
}

func (g *wrappedGraphStore) ApplyGraphPlanWithStorage(ctx context.Context, plan *beads.GraphApplyPlan, storage beads.StorageClass) (*beads.GraphApplyResult, error) {
	var result *beads.GraphApplyResult
	err := g.mutate(func() error {
		var err error
		result, err = g.GraphStore.ApplyGraphPlanWithStorage(ctx, plan, storage)
		return err
	})
	g.emitter.observe(ClassEventGraphApply, "ApplyGraphPlanWithStorage", err)
	return result, err
}

// WrapSessions puts the production stack in front of one Sessions binding.
func WrapSessions(inner SessionsStore, wrapping ClassWrapping) (SessionsStore, error) {
	if err := wrapping.validate(coordclass.ClassSessions); err != nil {
		return nil, err
	}
	if isNilInterface(inner) {
		return nil, refuseWrapping(coordclass.ClassSessions, "no front door to wrap")
	}
	return &wrappedSessionsStore{SessionsStore: inner, emitter: wrapping.emitter(coordclass.ClassSessions)}, nil
}

type wrappedSessionsStore struct {
	SessionsStore
	emitter classEmitter
}

func (s *wrappedSessionsStore) CreateSession(spec session.CreateSpec) (string, error) {
	id, err := s.SessionsStore.CreateSession(spec)
	s.emitter.observe(ClassEventWrite, "CreateSession", err)
	return id, err
}

// Tx is reported as a transaction rather than as a write, so an operator can
// tell a failed multi-write session rollback from a failed single patch — the
// same split wrappedGraphStore.Tx makes.
func (s *wrappedSessionsStore) Tx(message string, run func(session.Tx) error) error {
	err := s.SessionsStore.Tx(message, run)
	s.emitter.observe(ClassEventTransaction, "Tx", err)
	return err
}

func (s *wrappedSessionsStore) CreateSessionInfo(spec session.CreateSpec) (session.Info, error) {
	info, err := s.SessionsStore.CreateSessionInfo(spec)
	s.emitter.observe(ClassEventWrite, "CreateSessionInfo", err)
	return info, err
}

func (s *wrappedSessionsStore) Get(id string) (session.Info, error) {
	info, err := s.SessionsStore.Get(id)
	s.emitter.observe(ClassEventRead, "Get", err)
	return info, err
}

func (s *wrappedSessionsStore) ApplyPatch(id string, patch session.MetadataPatch) error {
	err := s.SessionsStore.ApplyPatch(id, patch)
	s.emitter.observe(ClassEventWrite, "ApplyPatch", err)
	return err
}

func (s *wrappedSessionsStore) SetState(id string, state session.State, reason string) error {
	err := s.SessionsStore.SetState(id, state, reason)
	s.emitter.observe(ClassEventWrite, "SetState", err)
	return err
}

// Close is the session close race: exactly one caller wins, and losing is not
// an error. It is reported as a claim so a lost race never reads as a failure.
func (s *wrappedSessionsStore) Close(id, reason string, at time.Time) (bool, error) {
	won, err := s.SessionsStore.Close(id, reason, at)
	s.emitter.observeClaim("Close", won, err)
	return won, err
}

func (s *wrappedSessionsStore) CreateWait(spec session.WaitSpec) (session.WaitInfo, error) {
	wait, err := s.SessionsStore.CreateWait(spec)
	s.emitter.observe(ClassEventWrite, "CreateWait", err)
	return wait, err
}

func (s *wrappedSessionsStore) CancelWait(id string, at time.Time, reason string) error {
	err := s.SessionsStore.CancelWait(id, at, reason)
	s.emitter.observe(ClassEventWrite, "CancelWait", err)
	return err
}

// WrapOrders puts the production stack in front of one Orders binding.
func WrapOrders(inner OrdersStore, wrapping ClassWrapping) (OrdersStore, error) {
	if err := wrapping.validate(coordclass.ClassOrders); err != nil {
		return nil, err
	}
	if isNilInterface(inner) {
		return nil, refuseWrapping(coordclass.ClassOrders, "no front door to wrap")
	}
	return &wrappedOrdersStore{OrdersStore: inner, emitter: wrapping.emitter(coordclass.ClassOrders)}, nil
}

type wrappedOrdersStore struct {
	OrdersStore
	emitter classEmitter
}

func (o *wrappedOrdersStore) CreateRun(order string, opts orders.RunOpts) (orders.OrderRun, error) {
	run, err := o.OrdersStore.CreateRun(order, opts)
	o.emitter.observe(ClassEventWrite, "CreateRun", err)
	return run, err
}

func (o *wrappedOrdersStore) CreateRunClosed(order string, outcome orders.RunOutcome, cursor *orders.EventCursor, reason string) (orders.OrderRun, error) {
	run, err := o.OrdersStore.CreateRunClosed(order, outcome, cursor, reason)
	o.emitter.observe(ClassEventWrite, "CreateRunClosed", err)
	return run, err
}

func (o *wrappedOrdersStore) Get(id string) (orders.OrderRun, error) {
	run, err := o.OrdersStore.Get(id)
	o.emitter.observe(ClassEventRead, "Get", err)
	return run, err
}

func (o *wrappedOrdersStore) SetOutcome(id string, outcome orders.RunOutcome) error {
	err := o.OrdersStore.SetOutcome(id, outcome)
	o.emitter.observe(ClassEventWrite, "SetOutcome", err)
	return err
}

func (o *wrappedOrdersStore) SetCursor(id, order string, cursor orders.EventCursor) error {
	err := o.OrdersStore.SetCursor(id, order, cursor)
	o.emitter.observe(ClassEventWrite, "SetCursor", err)
	return err
}

func (o *wrappedOrdersStore) CloseRun(id, reason string) error {
	err := o.OrdersStore.CloseRun(id, reason)
	o.emitter.observe(ClassEventWrite, "CloseRun", err)
	return err
}

func (o *wrappedOrdersStore) DeleteRun(id string) error {
	err := o.OrdersStore.DeleteRun(id)
	o.emitter.observe(ClassEventWrite, "DeleteRun", err)
	return err
}

func (o *wrappedOrdersStore) MarkFailed(id, reason string, outcome orders.RunOutcome, cursor *orders.EventCursor) error {
	err := o.OrdersStore.MarkFailed(id, reason, outcome, cursor)
	o.emitter.observe(ClassEventWrite, "MarkFailed", err)
	return err
}

// Retention sweeps are maintenance, not ordinary writes: they are the
// operations an operator schedules and an operator has to be able to see fail.
func (o *wrappedOrdersStore) CloseRuns(ctx context.Context, ids []string, reason string) (int, error) {
	closed, err := o.OrdersStore.CloseRuns(ctx, ids, reason)
	o.emitter.observe(ClassEventMaintenance, "CloseRuns", err)
	return closed, err
}

func (o *wrappedOrdersStore) CloseRunsSwept(ctx context.Context, ids []string, reason, sweep string) (int, error) {
	closed, err := o.OrdersStore.CloseRunsSwept(ctx, ids, reason, sweep)
	o.emitter.observe(ClassEventMaintenance, "CloseRunsSwept", err)
	return closed, err
}

// WrapNudges puts the production stack in front of one Nudges binding.
func WrapNudges(inner NudgeFrontDoors, wrapping ClassWrapping) (NudgeFrontDoors, error) {
	if err := wrapping.validate(coordclass.ClassNudges); err != nil {
		return NudgeFrontDoors{}, err
	}
	if !nudgeFrontDoorsUsable(inner) {
		return NudgeFrontDoors{}, refuseWrapping(coordclass.ClassNudges, "incomplete front-door set")
	}
	emitter := wrapping.emitter(coordclass.ClassNudges)
	return NudgeFrontDoors{
		Queue:   &wrappedNudgeQueue{NudgeQueue: inner.Queue, emitter: emitter},
		Shadows: &wrappedNudgeShadows{NudgeShadows: inner.Shadows, emitter: emitter},
	}, nil
}

type wrappedNudgeQueue struct {
	NudgeQueue
	emitter classEmitter
}

func (q *wrappedNudgeQueue) Enqueue(item nudgequeue.Item) error {
	err := q.NudgeQueue.Enqueue(item)
	q.emitter.observe(ClassEventWrite, "Enqueue", err)
	return err
}

func (q *wrappedNudgeQueue) EnqueueDeferred(item nudgequeue.Item) error {
	err := q.NudgeQueue.EnqueueDeferred(item)
	q.emitter.observe(ClassEventWrite, "EnqueueDeferred", err)
	return err
}

// ClaimDue is the queue's claim, and an empty batch is neither a failure nor a
// conflict: it is the ordinary state of an idle queue.
//
// This is a claim EVENT with an OK outcome, not observeClaim. A conflict means
// a compare-and-swap lost to another holder, and a poller against an empty
// queue lost nothing — reporting it as a conflict would make an idle fleet
// indistinguishable from a contended one on every dashboard that counts them,
// which is exactly the confusion the conflict outcome exists to prevent.
func (q *wrappedNudgeQueue) ClaimDue(target ClaimTarget, now time.Time) ([]nudgequeue.Item, error) {
	items, err := q.NudgeQueue.ClaimDue(target, now)
	q.emitter.observe(ClassEventClaim, "ClaimDue", err)
	return items, err
}

func (q *wrappedNudgeQueue) Ack(ids []string, sessionID, outcome, detail string) error {
	err := q.NudgeQueue.Ack(ids, sessionID, outcome, detail)
	q.emitter.observe(ClassEventWrite, "Ack", err)
	return err
}

func (q *wrappedNudgeQueue) ReleaseClaims(ids []string) error {
	err := q.NudgeQueue.ReleaseClaims(ids)
	q.emitter.observe(ClassEventWrite, "ReleaseClaims", err)
	return err
}

func (q *wrappedNudgeQueue) RecordFailure(ids []string, cause error, now time.Time) ([]nudgequeue.Item, error) {
	dead, err := q.NudgeQueue.RecordFailure(ids, cause, now)
	q.emitter.observe(ClassEventWrite, "RecordFailure", err)
	return dead, err
}

func (q *wrappedNudgeQueue) Rollback(item nudgequeue.Item, reason string) error {
	err := q.NudgeQueue.Rollback(item, reason)
	q.emitter.observe(ClassEventWrite, "Rollback", err)
	return err
}

func (q *wrappedNudgeQueue) Snapshot() (nudgequeue.State, error) {
	state, err := q.NudgeQueue.Snapshot()
	q.emitter.observe(ClassEventRead, "Snapshot", err)
	return state, err
}

type wrappedNudgeShadows struct {
	NudgeShadows
	emitter classEmitter
}

func (s *wrappedNudgeShadows) Save(item nudgequeue.Item) (string, bool, error) {
	id, created, err := s.NudgeShadows.Save(item)
	s.emitter.observe(ClassEventWrite, "Save", err)
	return id, created, err
}

func (s *wrappedNudgeShadows) Terminalize(item nudgequeue.Item, outcome, detail, sessionID string, at time.Time) error {
	err := s.NudgeShadows.Terminalize(item, outcome, detail, sessionID, at)
	s.emitter.observe(ClassEventWrite, "Terminalize", err)
	return err
}

// SweepStale retires one shadow bead. Its first argument is the SHADOW BEAD
// id Save minted, not the durable nudge id and not the agent queue key; the
// parameter is named for what it is because a caller that passes the wrong one
// gets a "bead not found" from three layers down.
func (s *wrappedNudgeShadows) SweepStale(beadID, closeReason string, before time.Time) error {
	err := s.NudgeShadows.SweepStale(beadID, closeReason, before)
	s.emitter.observe(ClassEventMaintenance, "SweepStale", err)
	return err
}

// WrapMessaging puts the production stack in front of one Messaging binding.
// The five services are wrapped independently because they are five contracts;
// the class they report is one.
func WrapMessaging(inner MessagingFrontDoors, wrapping ClassWrapping) (MessagingFrontDoors, error) {
	if err := wrapping.validate(coordclass.ClassMessaging); err != nil {
		return MessagingFrontDoors{}, err
	}
	if !messagingFrontDoorsUsable(inner) {
		return MessagingFrontDoors{}, refuseWrapping(coordclass.ClassMessaging, "incomplete front-door set")
	}
	emitter := wrapping.emitter(coordclass.ClassMessaging)
	return MessagingFrontDoors{
		Mail:             &wrappedMailProvider{Provider: inner.Mail, emitter: emitter},
		Bindings:         &wrappedBindingService{BindingService: inner.Bindings, emitter: emitter},
		DeliveryContexts: &wrappedDeliveryContextService{DeliveryContextService: inner.DeliveryContexts, emitter: emitter},
		Groups:           &wrappedGroupService{GroupService: inner.Groups, emitter: emitter},
		Transcripts:      &wrappedTranscriptService{TranscriptService: inner.Transcripts, emitter: emitter},
	}, nil
}

type wrappedMailProvider struct {
	mail.Provider
	emitter classEmitter
}

func (m *wrappedMailProvider) Send(from, to, subject, body string) (mail.Message, error) {
	message, err := m.Provider.Send(from, to, subject, body)
	m.emitter.observe(ClassEventWrite, "Send", err)
	return message, err
}

func (m *wrappedMailProvider) Get(id string) (mail.Message, error) {
	message, err := m.Provider.Get(id)
	m.emitter.observe(ClassEventRead, "Get", err)
	return message, err
}

func (m *wrappedMailProvider) Read(id string) (mail.Message, error) {
	message, err := m.Provider.Read(id)
	m.emitter.observe(ClassEventWrite, "Read", err)
	return message, err
}

func (m *wrappedMailProvider) MarkRead(id string) error {
	err := m.Provider.MarkRead(id)
	m.emitter.observe(ClassEventWrite, "MarkRead", err)
	return err
}

func (m *wrappedMailProvider) MarkUnread(id string) error {
	err := m.Provider.MarkUnread(id)
	m.emitter.observe(ClassEventWrite, "MarkUnread", err)
	return err
}

func (m *wrappedMailProvider) Archive(id string) error {
	err := m.Provider.Archive(id)
	m.emitter.observe(ClassEventWrite, "Archive", err)
	return err
}

func (m *wrappedMailProvider) Delete(id string) error {
	err := m.Provider.Delete(id)
	m.emitter.observe(ClassEventWrite, "Delete", err)
	return err
}

type wrappedBindingService struct {
	extmsg.BindingService
	emitter classEmitter
}

func (b *wrappedBindingService) Bind(ctx context.Context, caller extmsg.Caller, input extmsg.BindInput) (extmsg.SessionBindingRecord, error) {
	record, err := b.BindingService.Bind(ctx, caller, input)
	b.emitter.observe(ClassEventWrite, "Bind", err)
	return record, err
}

func (b *wrappedBindingService) Unbind(ctx context.Context, caller extmsg.Caller, input extmsg.UnbindInput) ([]extmsg.SessionBindingRecord, error) {
	records, err := b.BindingService.Unbind(ctx, caller, input)
	b.emitter.observe(ClassEventWrite, "Unbind", err)
	return records, err
}

type wrappedDeliveryContextService struct {
	extmsg.DeliveryContextService
	emitter classEmitter
}

func (d *wrappedDeliveryContextService) Record(ctx context.Context, caller extmsg.Caller, input extmsg.DeliveryContextRecord) error {
	err := d.DeliveryContextService.Record(ctx, caller, input)
	d.emitter.observe(ClassEventWrite, "Record", err)
	return err
}

type wrappedGroupService struct {
	extmsg.GroupService
	emitter classEmitter
}

func (g *wrappedGroupService) EnsureGroup(ctx context.Context, caller extmsg.Caller, input extmsg.EnsureGroupInput) (extmsg.ConversationGroupRecord, error) {
	record, err := g.GroupService.EnsureGroup(ctx, caller, input)
	g.emitter.observe(ClassEventWrite, "EnsureGroup", err)
	return record, err
}

type wrappedTranscriptService struct {
	extmsg.TranscriptService
	emitter classEmitter
}

func (t *wrappedTranscriptService) Append(ctx context.Context, input extmsg.AppendTranscriptInput) (extmsg.ConversationTranscriptRecord, error) {
	record, err := t.TranscriptService.Append(ctx, input)
	t.emitter.observe(ClassEventWrite, "Append", err)
	return record, err
}
