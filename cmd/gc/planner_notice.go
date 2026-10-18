package main

// Planner notices as conversation-lane ops (DES-O1 §9, OJ-14).
//
// The controller's planner reminders (the claim, continuation and execution
// backstops and the warm-bind claim nudge) keep their pacing markers, but
// when the controller's lane host executes message ops ([ops] authoritative
// lists message) they stop typing into the runtime themselves: each reminder
// becomes one message{notice, boundary, wake: live_only} op that the lane
// delivers at the session's next boundary. With the list empty, or with no
// lane host for the sessions store (a supervisor-hosted city), the reminders
// are delivered exactly as before.
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

// plannerNotice is one planner reminder to propose as a notice op.
type plannerNotice struct {
	// Purpose names the reminder (its journal label); it is the principal's
	// suffix and the client key's first field.
	Purpose string
	// Trigger is what the reminder is about (a work bead, a binding); it is
	// the op's causation and the client key's third field.
	Trigger string
	// Attempt is the reminder's attempt number, from 1.
	Attempt int
	// Content is the reminder text, delivered as a notice.
	Content string
	// StartWithin bounds how long the notice may wait for a boundary: the
	// reminder's own cadence, past which the next attempt replaces it.
	StartWithin time.Duration
}

// plannerNoticeOutcome is what proposePlannerNotice did.
type plannerNoticeOutcome int

const (
	// plannerNoticeProposed: the attempt's op is written (now or by an
	// earlier call with the same key).
	plannerNoticeProposed plannerNoticeOutcome = iota
	// plannerNoticePending: the previous attempt's op is still open and
	// due, so the reminder is still pending and nothing was written.
	plannerNoticePending
)

// plannerNoticeLane is the conversation lane a planner notice goes to: its op
// store, its clock, and the doorbell that starts a pass.
type plannerNoticeLane interface {
	noticeStore() (*opjournal.Store, error)
	noticeNow() time.Time
	Poke(conv string) bool
}

// plannerNoticeKey is the notice's client key,
// <purpose>:<instance token>:<trigger>:<attempt>. The instance token scopes
// it to one incarnation, so a respawned session is reminded afresh.
func plannerNoticeKey(n plannerNotice, token string, attempt int) string {
	return n.Purpose + ":" + token + ":" + n.Trigger + ":" + strconv.Itoa(attempt)
}

// plannerNoticeRequest is the op request for attempt n of notice to
// conversation conv, proposed at now.
func plannerNoticeRequest(conv, token string, n plannerNotice, now time.Time) opjournal.Request {
	req := opjournal.Request{
		Conversation: conv,
		Kind:         opjournal.KindMessage,
		Executor:     opjournal.ExecutorLane,
		ClientKey:    plannerNoticeKey(n, token, n.Attempt),
		Producer:     opjournal.Producer{Principal: plannerNoticePrincipalPrefix + n.Purpose, Actor: opjournal.ActorController},
		Payload:      n.Content,
		Policy: opjournal.Policy{Message: opjournal.MessagePolicy{
			Role: opjournal.RoleNotice, When: opjournal.WhenBoundary, Wake: opjournal.WakeLiveOnly,
		}},
		Causation: n.Trigger,
	}
	if n.StartWithin > 0 {
		req.StartBy = now.Add(n.StartWithin)
	}
	return req
}

// proposePlannerNotice proposes attempt n.Attempt of notice n to conversation
// conv, whose incarnation is token, and pokes the lane. It reads the time from
// the lane's clock, the one the op store stamps the op with. Before attempt n+1 it
// reads attempt n's op (one Get, no write): while that op is open and still
// due (executing, or not yet past its start_by), the reminder is pending and
// nothing is proposed. Proposing is idempotent by key, so a retry after a
// crash between the op and the producer's marker writes nothing new. The lane
// never drops a notice on the producer's behalf.
func proposePlannerNotice(lane plannerNoticeLane, conv, token string, n plannerNotice) (plannerNoticeOutcome, opjournal.Op, error) {
	if lane == nil {
		return 0, opjournal.Op{}, errors.New("proposing a planner notice: no conversation lane")
	}
	if n.Attempt < 1 {
		return 0, opjournal.Op{}, fmt.Errorf("proposing planner notice %s for %s: attempt %d is not >= 1", n.Purpose, conv, n.Attempt)
	}
	st, err := lane.noticeStore()
	if err != nil {
		return 0, opjournal.Op{}, fmt.Errorf("proposing planner notice %s for %s: %w", n.Purpose, conv, err)
	}
	now := lane.noticeNow()
	if n.Attempt > 1 {
		prev, pending, err := plannerNoticePrevPending(st, conv, token, n, now)
		if err != nil {
			return 0, opjournal.Op{}, err
		}
		if pending {
			return plannerNoticePending, prev, nil
		}
	}
	op, _, err := st.Propose(plannerNoticeRequest(conv, token, n, now))
	if err != nil {
		return 0, opjournal.Op{}, fmt.Errorf("proposing planner notice %s attempt %d for %s: %w", n.Purpose, n.Attempt, conv, err)
	}
	lane.Poke(conv)
	return plannerNoticeProposed, op, nil
}

// plannerNoticePrevPending reads attempt n.Attempt-1's op and reports whether
// it is still pending. A missing op is not pending.
func plannerNoticePrevPending(st *opjournal.Store, conv, token string, n plannerNotice, now time.Time) (opjournal.Op, bool, error) {
	id, err := opjournal.DeriveID(st.Prefix(), conv, plannerNoticePrincipalPrefix+n.Purpose, plannerNoticeKey(n, token, n.Attempt-1))
	if err != nil {
		return opjournal.Op{}, false, fmt.Errorf("deriving planner notice %s attempt %d for %s: %w", n.Purpose, n.Attempt-1, conv, err)
	}
	prev, err := st.Get(id)
	switch {
	case errors.Is(err, beads.ErrNotFound):
		return opjournal.Op{}, false, nil
	case err != nil:
		return opjournal.Op{}, false, fmt.Errorf("reading planner notice %s attempt %d for %s: %w", n.Purpose, n.Attempt-1, conv, err)
	}
	return prev, plannerNoticeStillDue(prev, now), nil
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

// plannerNoticeToken is a session row's incarnation token for a notice key.
func plannerNoticeToken(s beads.Bead) string {
	return strings.TrimSpace(s.Metadata["instance_token"])
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
