package main

// Planner notices as conversation-lane ops (DES-O1 §9, OJ-14).
//
// The controller's planner reminders (the claim, continuation and execution
// backstops and the warm-bind claim nudge) keep their pacing markers, but
// when the controller's lane host executes message ops ([ops] authoritative
// lists message) they stop typing into the runtime themselves: each reminder
// becomes one message{notice, boundary, wake: live_only} op that the lane
// delivers at the session's next boundary. With the list empty, without a
// lane host for the sessions store (a supervisor-hosted city), or for a
// session no lane host can type into (a protocol-mode runtime no other host
// serves), the reminders are delivered exactly as before.
//
// The lane host is found through the controller's lane-host registry by the
// sessions store the reminder writes its markers to, so the legacy reconciler
// and v2's C8, which call the same reminder functions, take the same route
// with no new parameter.
//
// The drain reminder stays on its direct delivery: its audience is a row in
// state=draining, which the lane's content gate (I7, session.ContentGate)
// blocks, so a drain-reminder notice op would never start.

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
	// between the op and the producer's marker). Nothing new was written.
	plannerNoticeAlreadyProposed
	// plannerNoticePending: an earlier notice for the same reminder is
	// still open and due, so nothing was written.
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

// plannerNoticeKey is the notice's client key,
// <purpose>:<token>:<trigger>[:<episode>]:<attempt>. The token scopes it to
// one incarnation, so a respawned session is reminded afresh, and the
// episode to one pacing episode of the incarnation.
func plannerNoticeKey(n plannerNotice, token string) string {
	parts := []string{n.Purpose, token, n.Trigger}
	if n.Episode != "" {
		parts = append(parts, n.Episode)
	}
	return strings.Join(append(parts, strconv.Itoa(n.Attempt)), ":")
}

// plannerNoticeRequest is the op request for notice n to conversation conv,
// incarnation token, proposed at now.
func plannerNoticeRequest(conv, token string, n plannerNotice, now time.Time) opjournal.Request {
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
		Causation: n.Causation,
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
// (one read): while an earlier notice for the same reminder in this
// incarnation is open and still due (executing, or not yet past its
// start_by), the reminder is pending and nothing is proposed, unless that
// notice is this very attempt, which is plannerNoticeAlreadyProposed.
// Proposing is idempotent by key, so a retry after a crash between the op
// and the producer's marker writes nothing new. The lane never drops a
// notice on the producer's behalf.
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
	serves, err := lane.servesNotice(st, strings.TrimSpace(s.Metadata["session_name"]), plannerNoticeView(s))
	if err != nil {
		return plannerNoticeNone, opjournal.Op{}, fmt.Errorf("proposing planner notice %s for %s: %w", n.Purpose, conv, err)
	}
	if !serves {
		return plannerNoticeDirect, opjournal.Op{}, nil
	}
	token, now := plannerNoticeToken(s), lane.noticeNow()
	req := plannerNoticeRequest(conv, token, n, now)
	id, err := opjournal.DeriveID(st.Prefix(), conv, req.Producer.Principal, req.ClientKey)
	if err != nil {
		return plannerNoticeNone, opjournal.Op{}, fmt.Errorf("deriving planner notice %s attempt %d for %s: %w", n.Purpose, n.Attempt, conv, err)
	}
	due, err := plannerNoticesDue(st, conv, token, n.Purpose, n.Causation, now)
	if err != nil {
		return plannerNoticeNone, opjournal.Op{}, err
	}
	if i := slices.IndexFunc(due, func(op opjournal.Op) bool { return op.ID == id }); i >= 0 {
		return plannerNoticeAlreadyProposed, due[i], nil
	}
	if len(due) > 0 {
		return plannerNoticePending, due[0], nil
	}
	op, created, err := st.Propose(req)
	if err != nil {
		return plannerNoticeNone, opjournal.Op{}, fmt.Errorf("proposing planner notice %s attempt %d for %s: %w", n.Purpose, n.Attempt, conv, err)
	}
	lane.Poke(conv)
	if !created {
		return plannerNoticeAlreadyProposed, op, nil
	}
	return plannerNoticeProposed, op, nil
}

// plannerNoticeOpen reports whether a notice of purpose about causation is
// still open and due in session row s's conversation, for its current
// incarnation: what holds a reminder's terminal action until its last
// notice has had its chance.
func plannerNoticeOpen(lane plannerNoticeLane, s beads.Bead, purpose, causation string) (bool, error) {
	st, err := lane.noticeStore()
	if err != nil {
		return false, fmt.Errorf("reading planner notices %s for %s: %w", purpose, s.ID, err)
	}
	due, err := plannerNoticesDue(st, s.ID, plannerNoticeToken(s), purpose, causation, lane.noticeNow())
	return len(due) > 0, err
}

// plannerNoticesDue are conv's open notices of purpose about causation for
// incarnation token that are still due, in CompareOrder order. It issues one
// read; a row the lane quarantines is the lane's to report, not a notice
// that holds a reminder.
func plannerNoticesDue(st *opjournal.Store, conv, token, purpose, causation string, now time.Time) ([]opjournal.Op, error) {
	ops, _, err := st.ListOpenTolerant(conv)
	if err != nil {
		return nil, fmt.Errorf("listing planner notices %s for %s: %w", purpose, conv, err)
	}
	principal, keyPrefix := plannerNoticePrincipalPrefix+purpose, purpose+":"+token+":"
	var due []opjournal.Op
	for _, op := range ops {
		if op.Producer.Principal == principal && op.Causation == causation &&
			strings.HasPrefix(op.ClientKey, keyPrefix) && plannerNoticeStillDue(op, now) {
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
	switch op.Status {
	case opjournal.StatusClosed:
		return false
	case opjournal.StatusExecuting, opjournal.StatusSettling:
		return true
	}
	return now.Before(op.StartBy)
}

// plannerNoticeRefused reports a proposal the op store refuses for the
// request itself, so retrying the same request can never succeed: an
// invalid request, an oversized payload, or a key reused with another
// request.
func plannerNoticeRefused(err error) bool {
	return errors.Is(err, opjournal.ErrInvalidRequest) || errors.Is(err, opjournal.ErrPayloadTooLarge) ||
		errors.Is(err, opjournal.ErrKeyReuse)
}

// plannerNoticeFailureKey names one reminder attempt in the lane's failure
// tally.
func plannerNoticeFailureKey(conv string, n plannerNotice) string {
	return conv + "\x00" + n.Purpose + "\x00" + n.Trigger + "\x00" + n.Episode + "\x00" + strconv.Itoa(n.Attempt)
}

// executesMessages reports whether the host is running and latched message
// in [ops] authoritative, so planner notices go to its lane.
func (h *conversationLaneHost) executesMessages() bool {
	if !slices.Contains(h.kinds, string(opjournal.KindMessage)) {
		return false
	}
	state, _ := h.stateReason()
	return state == laneHostStateRunning
}

// noticeStore is the host's op store, opened over its sessions store.
func (h *conversationLaneHost) noticeStore() (*opjournal.Store, error) {
	st, _, err := h.store()
	return st, err
}

// noticeNow is the host's clock, which its op store stamps ops with.
func (h *conversationLaneHost) noticeNow() time.Time { return h.clk.Now() }

// servesNotice reports whether a lane can type a notice into the session:
// the host's live executor can (laneTypesLive, the rule its caps follow),
// or the conversation's runtime row stamps another host serving message for
// the row's generation (hostedElsewhere), whose lane then delivers it.
func (h *conversationLaneHost) servesNotice(st *opjournal.Store, sessName string, view opjournal.ConversationView) (bool, error) {
	if laneTypesLive(h.env.SessionProvider(), sessName) {
		return true, nil
	}
	row, _, found, err := st.Runtime(view.ID)
	if err != nil {
		return false, fmt.Errorf("reading conversation %s's runtime row: %w", view.ID, err)
	}
	return found && hostedElsewhere(row.Host, view.Generation), nil
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
