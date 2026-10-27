package main

// Wait nudges on the conversation lane (design slice 8): when the
// controller's lane host for the sessions store executes message and serves
// the waiting session, a ready wait's nudge is a lane notice from gc:wait
// instead of a queue item: keyed wait:<waitNudgeID(wait)>, so each delivery
// attempt is its own op, caused by the wait, delivered at the boundary while
// the session runs (live_only: the wait's readiness already drives the
// wake), and scoped to the wait's registered continuation epoch when it
// names one. The wait's nudge_id then holds the op id, which its finalize
// and withdraw read (opjournal.IsOpID) in place of a queue shadow's. One
// path serves both reconcilers: dispatchReadyWaitNudgesWithSnapshot and
// finalizeReadyWaitFromNudge are their shared entry points. An attempt stays
// with the path that recorded it first across a flip or rollback of message
// in [ops] authoritative: a queued attempt gets no notice, and a noticed one
// no queue item (waitNoticeRecorded).

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/opjournal"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// waitNoticePrincipal is the principal of a wait's notice on the
// conversation lane; its label is wait.
const waitNoticePrincipal = "gc:wait"

// waitNoticeWithdrawReason is the reason a withdrawn wait notice's cancel
// records.
const waitNoticeWithdrawReason = "its wait was withdrawn"

// waitNoticeKey is the client key of the notice of wait nudge attempt
// nudgeID.
func waitNoticeKey(nudgeID string) string { return "wait:" + nudgeID }

// waitNoticeRequest is the lane notice of wait's nudge attempt nudgeID to
// conversation conv, carrying message.
func waitNoticeRequest(conv string, wait sessionpkg.WaitInfo, nudgeID, message string) opjournal.Request {
	req := cliLaneNotice(waitNoticePrincipal, wait.ID, message, opjournal.WakeLiveOnly)
	req.ClientKey = waitNoticeKey(nudgeID)
	req.Conversation = conv
	if epoch, err := strconv.ParseInt(strings.TrimSpace(wait.RegisteredEpoch), 10, 64); err == nil && epoch > 0 {
		req.Scope = opjournal.Scope{Kind: opjournal.ScopeContext, Epoch: epoch}
	}
	return req
}

// dispatchWaitNotice dispatches wait's nudge attempt nudgeID to session info
// on lane: an attempt whose op exists is queued (open) or resolved (closed)
// and proposes nothing; otherwise, when the lane serves the session, it
// proposes the notice, records its op id as the wait's nudge_id and rings
// the lane. It reports handled false, having written nothing, when the lane
// does not serve the session: the attempt takes the legacy queue.
func dispatchWaitNotice(lane plannerNoticeLane, sessFront *sessionpkg.Store, info sessionpkg.Info, wait sessionpkg.WaitInfo, nudgeID, message string) (bool, error) {
	st, err := lane.noticeStore()
	if err != nil {
		return false, fmt.Errorf("opening the lane store for wait %s: %w", wait.ID, err)
	}
	req := waitNoticeRequest(info.ID, wait, nudgeID, message)
	id, err := opjournal.DeriveID(st.Prefix(), info.ID, waitNoticePrincipal, req.ClientKey)
	if err != nil {
		return false, fmt.Errorf("deriving wait %s's notice: %w", wait.ID, err)
	}
	if _, err := st.Get(id); err == nil {
		if wait.NudgeID != id {
			return true, sessFront.SetWaitNudgeID(wait.ID, id)
		}
		return true, nil
	} else if !errors.Is(err, beads.ErrNotFound) {
		return false, fmt.Errorf("reading wait %s's notice %s: %w", wait.ID, id, err)
	}
	serves, err := lane.servesNotice(st, strings.TrimSpace(info.SessionNameMetadata), conversationLaneView(info))
	if err != nil {
		return false, fmt.Errorf("checking the lane for wait %s: %w", wait.ID, err)
	}
	if !serves {
		return false, nil
	}
	op, created, err := st.Propose(req)
	if err != nil {
		return false, fmt.Errorf("proposing wait %s's notice: %w", wait.ID, err)
	}
	if err := sessFront.SetWaitNudgeID(wait.ID, op.ID); err != nil {
		return true, fmt.Errorf("setting wait nudge_id: %w", err)
	}
	if created {
		lane.Poke(info.ID)
	}
	return true, nil
}

// waitNoticeRecorded reports whether wait's nudge attempt nudgeID is a lane
// notice: the wait's nudge_id is the op its attempt's key names in the op
// journal over sessFront's store, and that op exists, open or closed. The
// legacy queue then takes no item for the attempt: an open notice still
// drains after a rollback, and a closed one resolved the attempt.
func waitNoticeRecorded(sessFront *sessionpkg.Store, wait sessionpkg.WaitInfo, nudgeID string) (bool, error) {
	if !opjournal.IsOpID(wait.NudgeID) {
		return false, nil
	}
	st, err := opjournal.NewStore(sessFront.Store(), clock.Real{})
	if err != nil {
		return false, fmt.Errorf("opening the op journal for wait %s: %w", wait.ID, err)
	}
	id, err := opjournal.DeriveID(st.Prefix(), wait.SessionID, waitNoticePrincipal, waitNoticeKey(nudgeID))
	if err != nil {
		return false, fmt.Errorf("deriving wait %s's notice: %w", wait.ID, err)
	}
	if id != wait.NudgeID {
		return false, nil
	}
	switch _, err := st.Get(id); {
	case err == nil:
		return true, nil
	case errors.Is(err, beads.ErrNotFound):
		return false, nil
	default:
		return false, fmt.Errorf("reading wait %s's notice %s: %w", wait.ID, id, err)
	}
}

// finalizeWaitFromNotice closes ready wait from its lane notice opID: a
// notice that succeeded closes the wait, one that failed fails it with the
// failure's reason; a skipped or canceled notice leaves it (the epoch check,
// or the canceler, already ended the wait), and an open one keeps it ready.
func finalizeWaitFromNotice(sessFront *sessionpkg.Store, wait sessionpkg.WaitInfo, opID string, now time.Time) (bool, error) {
	st, err := opjournal.NewStore(sessFront.Store(), clock.Real{})
	if err != nil {
		return false, fmt.Errorf("opening the op journal for wait %s: %w", wait.ID, err)
	}
	op, err := st.Get(opID)
	switch {
	case errors.Is(err, beads.ErrNotFound):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("reading wait %s's notice %s: %w", wait.ID, opID, err)
	case op.Status != opjournal.StatusClosed:
		return false, nil
	}
	arm, reason := op.Outcome.Effective()
	switch arm {
	case opjournal.ArmSucceeded:
		return true, sessFront.CloseWaitFromNudge(wait.ID, now, opID, "")
	case opjournal.ArmFailed:
		return true, sessFront.FailWaitFromNudge(wait.ID, now, opID, reason, "")
	}
	return false, nil
}

// splitWaitNudgeIDs splits a wait's nudge ids into lane notice op ids and
// legacy queue ids.
func splitWaitNudgeIDs(ids []string) (ops, queued []string) {
	for _, id := range ids {
		if opjournal.IsOpID(id) {
			ops = append(ops, id)
		} else {
			queued = append(queued, id)
		}
	}
	return ops, queued
}

// withdrawWaitNotices cancels wait notice ops ids in the op journal over
// sessions, as gc:wait.
func withdrawWaitNotices(sessions beads.SessionStore, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	st, err := opjournal.NewStore(sessions, clock.Real{})
	if err != nil {
		return fmt.Errorf("opening the op journal to withdraw wait notices: %w", err)
	}
	var errs []error
	for _, id := range ids {
		if _, _, err := st.Cancel(id, waitNoticePrincipal, waitNoticeWithdrawReason); err != nil && !errors.Is(err, beads.ErrNotFound) {
			errs = append(errs, fmt.Errorf("withdrawing wait notice %s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}
