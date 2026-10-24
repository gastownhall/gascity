package main

// Planner notices as conversation-lane ops (DES-O1 §9, OJ-14).
//
// The controller's planner reminders (the claim, continuation and execution
// backstops, the warm-bind claim nudge and the drain reminder) keep their pacing markers, but
// when the controller's lane host executes message ops ([ops] authoritative
// lists message) they stop typing into the runtime themselves: each reminder
// becomes one message{notice, boundary, wake: live_only} op that the lane
// delivers at the session's next boundary. With the list empty, without a
// running lane host for the sessions store, or for a session no lane host
// can type into (a protocol-mode runtime no other host serves), the
// reminders are delivered exactly as before.
//
// The lane host is found through the controller's lane-host registry by the
// sessions store the reminder writes its markers to, so the legacy reconciler
// and v2's C8, which call the same reminder functions, take the same route
// with no new parameter.
//
// A reminder's notices share one correlation, its purpose, incarnation and
// trigger. Only one of them stands at a time: a new attempt cancels an
// earlier one that has not started, and waits while one is in flight. An
// Exclusive purpose (the warm-bind claim nudge: a slot holds one binding at a
// time) keeps one notice per conversation, whatever its trigger or
// incarnation.
//
// A reminder whose cause clears or moves withdraws its open notices,
// best-effort and once, on the tick that writes the marker saying so: a
// backstop's clear withdraws its own, a backstop that observes a new target
// its own for the target it replaces, and a started execution seat the
// execution backstop's. The warm-bind claim nudge's notice is withdrawn by the
// claim backstop's clear and by a started execution seat, through the record
// the warm bind writes when it proposes one (withdrawWarmBindNotice), so a
// trigger claimed before the claim backstop first saw it is covered too. A
// notice the lane already started keeps the cancel as a request for the lane
// to decide. A stale notice the withdraw misses stands until its start_by,
// within one cadence of its reminder: after a withdraw whose write failed, or
// in a v2 city on the work ladder (where the claim and execution backstops do
// not run).
//
// A notice is scoped to its session's generation as well as keyed to its
// incarnation, so one a respawn leaves open closes as stale rather than
// reaching the next incarnation.
//
// The drain reminder is a planner notice too, though its audience is a row
// in state=draining, which the lane's content gate (I7, session.ContentGate)
// holds all other content from: the gate admits the drain reminder's notice
// (session.GateContentDrainNotice, laneDrainNoticeClass) and nothing else. A
// row the gate holds the notice from for another reason is nudged directly
// (deliverDrainReminder).
//
// A conversation an operator holds (its content_hold cell,
// session.ContentHeldInfo) holds every notice, the drain reminder's included
// (SESSION-RUNTIME-027), so the backstops and the drain reminder do not
// remind it by either route: a backstop that would act on it spends no
// attempt and takes no terminal action (runNudgeBackstop), and a due drain
// reminder spends no attempt (drainReminderContentHeld), until the hold
// clears.

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/opjournal"
)

// plannerNoticePrincipalPrefix starts every planner notice's principal,
// gc:planner/<purpose>. The lane renders its source label as "planner".
const plannerNoticePrincipalPrefix = "gc:planner/"

// plannerNoticeStartMargin is how much sooner than its reminder's cadence a
// notice's start_by falls (plannerNoticeStartWithin). The pacing time is
// stored to the second, so the next attempt can come up to a second before
// one full cadence after this one's proposal; the margin puts this attempt's
// start_by safely before that, so the next attempt finds it expired or in
// flight, never still waiting.
const plannerNoticeStartMargin = 15 * time.Second

// plannerNoticeMaxFailures is how many proposals of one notice may fail in a
// row before its attempt is spent anyway, so a journal that cannot be written
// still lets the reminder reach its terminal action.
const plannerNoticeMaxFailures = 3

// plannerNotice is one planner reminder to propose as a notice op.
type plannerNotice struct {
	// Purpose names the reminder (its journal label); it is the principal's
	// suffix and the client key's first field.
	Purpose string
	// Trigger is what the reminder is about, with whatever provenance tells
	// two same-id triggers apart; it is the client key's third field.
	Trigger string
	// Causation is the bead the reminder is about, the op's causation.
	Causation string
	// Episode names the reminder's pacing episode, so a trigger cleared and
	// observed again in one incarnation is reminded afresh: the backstop's
	// pacing time. "" for a reminder that fires once per trigger.
	Episode string
	// Attempt is the reminder's attempt number, from 1.
	Attempt int
	// Content is the reminder text, delivered as a notice.
	Content string
	// StartWithin bounds how long the notice may wait for a boundary
	// (plannerNoticeStartWithin of the reminder's cadence), past which the
	// next attempt replaces it.
	StartWithin time.Duration
	// Exclusive makes the notice its purpose's only one in the conversation:
	// proposing it supersedes, or waits on, the purpose's other notices
	// whatever their trigger or incarnation, not only its reminder's.
	Exclusive bool
}

// plannerNoticeOutcome is what proposePlannerNotice did.
type plannerNoticeOutcome int

const (
	// plannerNoticeNone is the zero value; proposePlannerNotice returns it
	// only beside an error.
	plannerNoticeNone plannerNoticeOutcome = iota
	// plannerNoticeProposed: the attempt's op was written now.
	plannerNoticeProposed
	// plannerNoticeAlreadyProposed: the attempt's op was already written,
	// by an earlier call that did not get to spend the attempt (a crash
	// between the op and the producer's marker), perhaps with a reminder
	// text since edited. Nothing new was written.
	plannerNoticeAlreadyProposed
	// plannerNoticePending: an earlier notice for the same reminder (for an
	// Exclusive notice, of its purpose) is in flight, so nothing was written.
	plannerNoticePending
	// plannerNoticeDirect: no lane host can type into the session, so the
	// caller delivers the reminder itself. Nothing was written.
	plannerNoticeDirect
)

// plannerNoticeLane is the conversation lane a planner notice goes to: its op
// store, its clock, the doorbell that starts a pass, whether it can serve a
// session, and the failure tally that rate-limits a notice's failures.
type plannerNoticeLane interface {
	noticeStore() (*opjournal.Store, error)
	noticeNow() time.Time
	Poke(conv string) bool
	// servesNotice reports whether a lane can type a notice into the
	// session named sessName, whose row's view is view.
	servesNotice(st *opjournal.Store, sessName string, view opjournal.ConversationView) (bool, error)
	// noticeFailed tallies one more failure for key and reports the count
	// and whether to log it now (at most once per laneErrorLogInterval).
	noticeFailed(key string) (failures int, logNow bool)
	// noticeRecovered clears key's tally.
	noticeRecovered(key string)
}

// plannerNoticeStartWithin is a notice's start_by window for a reminder
// whose next attempt comes one cadence later: the cadence less
// plannerNoticeStartMargin, or half a cadence too short for the margin.
func plannerNoticeStartWithin(cadence time.Duration) time.Duration {
	if cadence > 2*plannerNoticeStartMargin {
		return cadence - plannerNoticeStartMargin
	}
	return cadence / 2
}

// plannerNoticeEpisode is the Episode of a backstop attempt paced from last:
// its Unix second, which the backstop's marker stores and so reads back the
// same after a crash.
func plannerNoticeEpisode(last time.Time) string {
	return strconv.FormatInt(last.Unix(), 10)
}

// plannerNoticeCorrelation is the correlation every notice of one reminder
// carries, <purpose>:<token>:<trigger>: the reminder for one trigger in one
// incarnation, whatever its episode or attempt. It is matched whole, never by
// prefix, because a trigger's provenance may itself contain ':'.
func plannerNoticeCorrelation(n plannerNotice, token string) string {
	return strings.Join([]string{n.Purpose, token, n.Trigger}, ":")
}

// plannerNoticeKey is the notice's client key,
// <purpose>:<token>:<trigger>[:<episode>]:<attempt>, its correlation then its
// episode and attempt. The token scopes it to one incarnation, so a respawned
// session is reminded afresh, and the episode to one pacing episode of the
// incarnation.
func plannerNoticeKey(n plannerNotice, token string) string {
	parts := []string{plannerNoticeCorrelation(n, token)}
	if n.Episode != "" {
		parts = append(parts, n.Episode)
	}
	return strings.Join(append(parts, strconv.Itoa(n.Attempt)), ":")
}

// plannerNoticeRequest is the op request for notice n to conversation conv,
// incarnation token of session generation gen (at least 1, as a conversation
// view reads it), proposed at now. The op is scoped to that generation, so
// the lane closes it as stale once the session respawns.
func plannerNoticeRequest(conv, token string, gen int64, n plannerNotice, now time.Time) opjournal.Request {
	req := opjournal.Request{
		Conversation: conv,
		Kind:         opjournal.KindMessage,
		Executor:     opjournal.ExecutorLane,
		ClientKey:    plannerNoticeKey(n, token),
		Producer:     opjournal.Producer{Principal: plannerNoticePrincipalPrefix + n.Purpose, Actor: opjournal.ActorController},
		Payload:      n.Content,
		Policy: opjournal.Policy{Message: opjournal.MessagePolicy{
			Role: opjournal.RoleNotice, When: opjournal.WhenBoundary, Wake: opjournal.WakeLiveOnly,
		}},
		Scope:       opjournal.Scope{Kind: opjournal.ScopeSession, Generation: gen},
		Causation:   n.Causation,
		Correlation: plannerNoticeCorrelation(n, token),
	}
	if n.StartWithin > 0 {
		req.StartBy = now.Add(n.StartWithin)
	}
	return req
}

// proposePlannerNotice proposes notice n to session row s's conversation and
// pokes the lane. It reads the time from the lane's clock, the one the op
// store stamps the op with.
//
// When no lane can type into the session it writes nothing and returns
// plannerNoticeDirect. Otherwise it first lists the conversation's open ops
// (one read) for the reminder's notices in this incarnation that are still
// due (plannerNoticesDue, by correlation, so a same-id trigger from another
// store, root or generation is not this reminder; for an Exclusive notice,
// every notice of its purpose):
//   - this very attempt among them is plannerNoticeAlreadyProposed;
//     proposing is idempotent by key, so a retry after a crash between the
//     op and the producer's marker writes nothing new;
//   - one in flight leaves the reminder pending, and nothing is proposed;
//   - any other, not yet started, is superseded (supersedePlannerNotices):
//     canceled before this attempt is proposed, so an earlier episode's or
//     an earlier text's notice never reaches the boundary after it.
//
// An attempt whose op was written with a text since edited is that op,
// plannerNoticeAlreadyProposed (proposePlannerNoticeOp).
func proposePlannerNotice(lane plannerNoticeLane, s beads.Bead, n plannerNotice) (plannerNoticeOutcome, opjournal.Op, error) {
	if lane == nil {
		return plannerNoticeNone, opjournal.Op{}, errors.New("proposing a planner notice: no conversation lane")
	}
	conv := s.ID
	if n.Attempt < 1 {
		return plannerNoticeNone, opjournal.Op{}, fmt.Errorf("proposing planner notice %s for %s: attempt %d is not >= 1", n.Purpose, conv, n.Attempt)
	}
	st, err := lane.noticeStore()
	if err != nil {
		return plannerNoticeNone, opjournal.Op{}, fmt.Errorf("proposing planner notice %s for %s: %w", n.Purpose, conv, err)
	}
	view := plannerNoticeView(s)
	serves, err := lane.servesNotice(st, strings.TrimSpace(s.Metadata["session_name"]), view)
	if err != nil {
		return plannerNoticeNone, opjournal.Op{}, fmt.Errorf("proposing planner notice %s for %s: %w", n.Purpose, conv, err)
	}
	if !serves {
		return plannerNoticeDirect, opjournal.Op{}, nil
	}
	token, now := plannerNoticeToken(s), lane.noticeNow()
	req := plannerNoticeRequest(conv, token, view.Generation, n, now)
	id, err := opjournal.DeriveID(st.Prefix(), conv, req.Producer.Principal, req.ClientKey)
	if err != nil {
		return plannerNoticeNone, opjournal.Op{}, fmt.Errorf("deriving planner notice %s attempt %d for %s: %w", n.Purpose, n.Attempt, conv, err)
	}
	due, err := plannerNoticesDue(st, conv, req.Producer.Principal, plannerNoticeScope(n, req.Correlation), now)
	if err != nil {
		return plannerNoticeNone, opjournal.Op{}, err
	}
	if i := slices.IndexFunc(due, func(op opjournal.Op) bool { return op.ID == id }); i >= 0 {
		return plannerNoticeAlreadyProposed, due[i], nil
	}
	if i := slices.IndexFunc(due, plannerNoticeInFlight); i >= 0 {
		return plannerNoticePending, due[i], nil
	}
	started, raced, err := supersedePlannerNotices(st, due, id, req.Producer.Principal)
	if err != nil {
		return plannerNoticeNone, opjournal.Op{}, fmt.Errorf("superseding planner notices %s for %s: %w", n.Purpose, conv, err)
	}
	if raced {
		return plannerNoticePending, started, nil
	}
	op, created, err := proposePlannerNoticeOp(st, req)
	if err != nil {
		return plannerNoticeNone, opjournal.Op{}, fmt.Errorf("proposing planner notice %s attempt %d for %s: %w", n.Purpose, n.Attempt, conv, err)
	}
	lane.Poke(conv)
	if !created {
		return plannerNoticeAlreadyProposed, op, nil
	}
	return plannerNoticeProposed, op, nil
}

// plannerNoticeScope is the correlation of the notices that proposing n
// waits on or supersedes: its reminder's, or "" (every notice of its
// purpose) for an Exclusive notice.
func plannerNoticeScope(n plannerNotice, correlation string) string {
	if n.Exclusive {
		return ""
	}
	return correlation
}

// proposePlannerNoticeOp proposes req. A key already written with another
// request (opjournal.KeyReuseError: the digest leaves out deadlines, and a
// notice's kind and policy are fixed, so another reminder text) is that op,
// not created: the attempt's op stands, and its proposal is spent.
func proposePlannerNoticeOp(st *opjournal.Store, req opjournal.Request) (opjournal.Op, bool, error) {
	op, created, err := st.Propose(req)
	if reused := (*opjournal.KeyReuseError)(nil); errors.As(err, &reused) {
		return reused.Existing, false, nil
	}
	return op, created, err
}

// plannerNoticeOpen reports whether a notice of reminder n (its purpose and
// trigger, any episode or attempt) is still open and due in session row s's
// conversation, for its current incarnation: what holds a reminder's
// terminal action until its last notice has had its chance.
func plannerNoticeOpen(lane plannerNoticeLane, s beads.Bead, n plannerNotice) (bool, error) {
	st, err := lane.noticeStore()
	if err != nil {
		return false, fmt.Errorf("reading planner notices %s for %s: %w", n.Purpose, s.ID, err)
	}
	due, err := plannerNoticesDue(st, s.ID, plannerNoticePrincipalPrefix+n.Purpose,
		plannerNoticeCorrelation(n, plannerNoticeToken(s)), lane.noticeNow())
	return len(due) > 0, err
}

// plannerNoticeFate is what became of one attempt's notice op.
type plannerNoticeFate int

const (
	// plannerNoticeFateNone: the attempt has no op (it was typed directly,
	// or its proposal never landed), so nothing more is known of it.
	plannerNoticeFateNone plannerNoticeFate = iota
	// plannerNoticeFateDelivered: the lane delivered it.
	plannerNoticeFateDelivered
	// plannerNoticeFateUndelivered: it closed without a delivery (expired,
	// canceled, superseded or failed), or stands open past its start_by,
	// which the lane closes as expired.
	plannerNoticeFateUndelivered
	// plannerNoticeFateStands: it is in flight, or open and still due.
	plannerNoticeFateStands
)

// plannerNoticeAttemptFate reads notice n's attempt op in session row s's
// conversation, for its current incarnation, and reports its fate at now.
// It issues one read.
func plannerNoticeAttemptFate(lane plannerNoticeLane, s beads.Bead, n plannerNotice, now time.Time) (plannerNoticeFate, error) {
	st, err := lane.noticeStore()
	if err != nil {
		return plannerNoticeFateNone, fmt.Errorf("reading planner notice %s attempt %d for %s: %w", n.Purpose, n.Attempt, s.ID, err)
	}
	req := plannerNoticeRequest(s.ID, plannerNoticeToken(s), plannerNoticeView(s).Generation, n, now)
	id, err := opjournal.DeriveID(st.Prefix(), s.ID, req.Producer.Principal, req.ClientKey)
	if err != nil {
		return plannerNoticeFateNone, fmt.Errorf("deriving planner notice %s attempt %d for %s: %w", n.Purpose, n.Attempt, s.ID, err)
	}
	op, err := st.Get(id)
	switch {
	case errors.Is(err, beads.ErrNotFound):
		return plannerNoticeFateNone, nil
	case err != nil:
		return plannerNoticeFateNone, fmt.Errorf("reading planner notice %s attempt %d for %s: %w", n.Purpose, n.Attempt, s.ID, err)
	case op.Status == opjournal.StatusClosed:
		if arm, _ := op.Outcome.Effective(); arm == opjournal.ArmSucceeded {
			return plannerNoticeFateDelivered, nil
		}
		return plannerNoticeFateUndelivered, nil
	case plannerNoticeStillDue(op, now):
		return plannerNoticeFateStands, nil
	}
	return plannerNoticeFateUndelivered, nil
}

// plannerNoticesDue are conv's open notices from principal with correlation
// (one reminder's, plannerNoticeCorrelation; "" for any) that are still due,
// in CompareOrder order. It issues one read; a row the lane quarantines is
// the lane's to report, not a notice that holds a reminder.
func plannerNoticesDue(st *opjournal.Store, conv, principal, correlation string, now time.Time) ([]opjournal.Op, error) {
	ops, _, err := st.ListOpenTolerant(conv)
	if err != nil {
		return nil, fmt.Errorf("listing planner notices from %s for %s: %w", principal, conv, err)
	}
	var due []opjournal.Op
	for _, op := range ops {
		if op.Producer.Principal == principal && (correlation == "" || op.Correlation == correlation) && plannerNoticeStillDue(op, now) {
			due = append(due, op)
		}
	}
	return due, nil
}

// plannerNoticeStillDue reports whether an earlier attempt still stands for
// the reminder: open, and either in flight or not yet past its start_by. An
// attempt past its start_by that never started is one the lane closes as
// expired, so the next attempt replaces it.
func plannerNoticeStillDue(op opjournal.Op, now time.Time) bool {
	switch {
	case op.Status == opjournal.StatusClosed:
		return false
	case plannerNoticeInFlight(op):
		return true
	}
	return now.Before(op.StartBy)
}

// plannerNoticeInFlight reports whether a lane has started notice op: it is
// being delivered, or its outcome is being decided.
func plannerNoticeInFlight(op opjournal.Op) bool {
	return op.Status == opjournal.StatusExecuting || op.Status == opjournal.StatusSettling
}

// supersedePlannerNotices cancels due, a reminder's earlier notices that have
// not started, as principal, ahead of proposing its attempt id. When op id is
// already stored (a retry, whose proposal writes nothing new) it cancels
// nothing, so no notice is withdrawn without a successor. A notice the lane
// started before its cancel landed keeps the cancel as a request for the lane
// to decide, and is returned with true: the reminder waits on it.
func supersedePlannerNotices(st *opjournal.Store, due []opjournal.Op, id, principal string) (opjournal.Op, bool, error) {
	if len(due) == 0 {
		return opjournal.Op{}, false, nil
	}
	switch _, err := st.Get(id); {
	case err == nil:
		return opjournal.Op{}, false, nil
	case !errors.Is(err, beads.ErrNotFound):
		return opjournal.Op{}, false, err
	}
	for _, old := range due {
		result, op, err := st.Cancel(old.ID, principal, "superseded by "+id)
		if err != nil {
			return opjournal.Op{}, false, fmt.Errorf("canceling %s: %w", old.ID, err)
		}
		if result == opjournal.CancelRequested || result == opjournal.CancelAlreadyDecided {
			return op, true, nil
		}
	}
	return opjournal.Op{}, false, nil
}

// withdrawPlannerNotices cancels every open notice of purpose in session row
// s's conversation, whatever its trigger or incarnation, because the
// reminder's cause has cleared or moved (reason says how). A notice already
// in flight keeps the cancel as a request for the lane to decide. It issues
// one read, and writes only for an open notice.
func withdrawPlannerNotices(lane plannerNoticeLane, s beads.Bead, purpose, reason string) error {
	st, err := lane.noticeStore()
	if err != nil {
		return fmt.Errorf("withdrawing planner notices %s for %s: %w", purpose, s.ID, err)
	}
	ops, _, err := st.ListOpenTolerant(s.ID)
	if err != nil {
		return fmt.Errorf("withdrawing planner notices %s for %s: %w", purpose, s.ID, err)
	}
	principal := plannerNoticePrincipalPrefix + purpose
	var errs []error
	for _, op := range ops {
		if op.Producer.Principal != principal {
			continue
		}
		if _, _, err := st.Cancel(op.ID, principal, reason); err != nil {
			errs = append(errs, fmt.Errorf("withdrawing planner notice %s for %s: %w", op.ID, s.ID, err))
		}
	}
	return errors.Join(errs...)
}

// plannerNoticeRefused reports a proposal the op store refuses for the
// request itself, so retrying the same request can never succeed: an
// invalid request or an oversized payload.
func plannerNoticeRefused(err error) bool {
	return errors.Is(err, opjournal.ErrInvalidRequest) || errors.Is(err, opjournal.ErrPayloadTooLarge)
}

// plannerNoticeFailureKey names one reminder attempt in the lane's failure
// tally.
func plannerNoticeFailureKey(conv string, n plannerNotice) string {
	return plannerNoticeTallyKey(conv, n, n.Episode, strconv.Itoa(n.Attempt))
}

// plannerNoticeTallyKey names an action on conv's reminder n in the lane's
// failure tally: its purpose and trigger, then the fields that name the
// action (an attempt's episode and number, or a read's name).
func plannerNoticeTallyKey(conv string, n plannerNotice, action ...string) string {
	return strings.Join(append([]string{conv, n.Purpose, n.Trigger}, action...), "\x00")
}

// executesMessages reports whether the host is running and latched message
// in [ops] authoritative, so planner notices go to its lane.
func (h *conversationLaneHost) executesMessages() bool {
	return slices.Contains(h.runningKinds(), string(opjournal.KindMessage))
}

// noticeStore is the host's op store, opened over its sessions store.
func (h *conversationLaneHost) noticeStore() (*opjournal.Store, error) {
	st, _, err := h.store()
	return st, err
}

// noticeNow is the host's clock, which its op store stamps ops with.
func (h *conversationLaneHost) noticeNow() time.Time { return h.clk.Now() }

// servesNotice reports whether a lane can type a notice into the session:
// the host's live executor can (laneTypesLive, its caps' rule for a
// keystroke-mode runtime), or the conversation's runtime row stamps another
// host serving message for the row's generation (hostedElsewhere), whose
// lane then delivers it. A protocol-mode session is nudged directly, as
// before, even where this host's executor serves it from its turns.
func (h *conversationLaneHost) servesNotice(st *opjournal.Store, sessName string, view opjournal.ConversationView) (bool, error) {
	return laneServesSession(h.env.SessionProvider(), st, sessName, view)
}

// noticeFailed implements plannerNoticeLane.
func (h *conversationLaneHost) noticeFailed(key string) (int, bool) {
	now := h.clk.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.noticeFailures == nil {
		h.noticeFailures = map[string]*laneNoticeFailures{}
	}
	f := h.noticeFailures[key]
	if f == nil {
		f = &laneNoticeFailures{}
		h.noticeFailures[key] = f
	}
	f.count++
	logNow := f.logged.IsZero() || !now.Before(f.logged.Add(laneErrorLogInterval))
	if logNow {
		f.logged = now
	}
	return f.count, logNow
}

// noticeRecovered implements plannerNoticeLane.
func (h *conversationLaneHost) noticeRecovered(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.noticeFailures, key)
}

// laneNoticeFailures is one planner notice's failure tally: failures in a
// row, and when one was last logged.
type laneNoticeFailures struct {
	count  int
	logged time.Time
}

// plannerNoticeLaneFor returns the lane planner notices for the session rows
// in store go to: the registered lane host that executes message ops over the
// same sessions store. nil means deliver as before.
func plannerNoticeLaneFor(store beads.Store) plannerNoticeLane {
	if isNilBeadStore(store) {
		return nil
	}
	var found *conversationLaneHost
	conversationLaneHosts.Range(func(_, v any) bool {
		h, ok := v.(*conversationLaneHost)
		if !ok || !h.executesMessages() {
			return true
		}
		if laneStoresShare(h.env.SessionsBeadStore().Store, store) {
			found = h
			return false
		}
		return true
	})
	if found == nil {
		return nil
	}
	return found
}

// laneStoreWrapHops bounds the wrapper walk in laneStoresShare.
const laneStoreWrapHops = 8

// laneStoresShare reports whether a and b are, or wrap, the same store.
func laneStoresShare(a, b beads.Store) bool {
	for sa, i := a, 0; i < laneStoreWrapHops && !isNilBeadStore(sa); sa, i = laneStoreWrapped(sa), i+1 {
		for sb, j := b, 0; j < laneStoreWrapHops && !isNilBeadStore(sb); sb, j = laneStoreWrapped(sb), j+1 {
			if sameBeadStore(sa, sb) {
				return true
			}
		}
	}
	return false
}

// laneStoreWrapped is the store store wraps, nil when it wraps none: a class
// view's store, the lane's live view's store, a cache's backing, or a
// wrapper's conditional-writes target.
func laneStoreWrapped(store beads.Store) beads.Store {
	switch s := store.(type) {
	case beads.SessionStore:
		return s.Store
	case laneLiveSessions:
		return s.Store
	}
	return laneUnwrapStore(store)
}

// plannerNoticeView is session row s's conversation view.
func plannerNoticeView(s beads.Bead) opjournal.ConversationView {
	return conversationLaneView(sessionInfoFromBead(s))
}

// plannerNoticeToken is a session row's incarnation for a notice key: its
// instance token, or, on a row that carries none, g<generation>, so the key
// is still scoped to one incarnation.
func plannerNoticeToken(s beads.Bead) string {
	if token := strings.TrimSpace(s.Metadata["instance_token"]); token != "" {
		return token
	}
	return "g" + strconv.FormatInt(plannerNoticeView(s).Generation, 10)
}

// backstopNoticeTrigger is a backstop target's notice trigger: the work bead
// id, then whichever of its store, root and generation the target carries,
// so the same id in independent stores or a recycled root or generation is
// another trigger, as it is another pacing state.
func backstopNoticeTrigger(t backstopTarget) string {
	parts := []string{t.ID}
	for _, p := range []string{t.StoreRef, t.RootID, t.Generation} {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, "/")
}
