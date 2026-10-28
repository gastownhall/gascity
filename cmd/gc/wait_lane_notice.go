package main

// Wait nudges on the conversation lane (design slice 8): when the
// controller's lane host for the sessions store executes message and serves
// the waiting session, a ready wait's nudge is a lane notice from gc:wait
// instead of a queue item: keyed wait:<waitNudgeID(wait)>, so each delivery
// attempt is its own op, caused by the wait, delivered at the boundary while
// the session runs (live_only: the wait's readiness already drives the
// wake), and scoped to the wait's registered continuation epoch when it
// names one. The wait's nudge_id then holds the op id, recorded before the
// op is proposed, which its finalize, withdraw and re-arm read
// (opjournal.IsOpID) in place of a queue shadow's. The notice's outcome ends
// the wait, but for the withdrawal of a wait re-armed since, which moves it
// to its next attempt. While the notice is open, a re-arm of the same wait
// keeps its attempt, and a retry of a closed wait is refused until the
// notice closes. One path serves both reconcilers:
// dispatchReadyWaitNudgesWithSnapshot and finalizeReadyWaitFromNudge are
// their shared entry points. An attempt stays with the path that recorded it
// first across a flip or rollback of message in [ops] authoritative: a queued
// attempt gets no notice, and a noticed one no queue item
// (waitNoticeRecorded). The queue records its item as nudge_id before it
// queues it, as the lane does its op before proposing it, so a withdrawal,
// which reads nudge_id, finds whichever of the two can be live. A nudge_id
// naming a notice that was never proposed holds no delivery, and the
// readers then read the attempt's queue item.

import (
	"errors"
	"fmt"
	"log"
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
// records the notice's op id as the wait's nudge_id, then proposes the
// notice and rings the lane. Recording first means no op exists that its
// wait does not name: a crash between the two writes leaves a nudge_id that
// names no op, which reads as not found everywhere, so the next pass
// proposes the notice, or after a rollback records the attempt's queue item
// in its place and queues it. It reports handled false, having written
// nothing, when the lane does not serve the session or the op store refuses
// the notice itself (a note past the inline payload limit, or not UTF-8):
// the attempt takes the legacy queue. An error leaves the attempt to the
// next pass.
func dispatchWaitNotice(lane plannerNoticeLane, sessFront *sessionpkg.Store, info sessionpkg.Info, wait sessionpkg.WaitInfo, nudgeID, message string) (bool, error) {
	st, err := lane.noticeStore()
	if err != nil {
		return false, fmt.Errorf("opening the lane store for wait %s: %w", wait.ID, err)
	}
	req := waitNoticeRequest(info.ID, wait, nudgeID, message)
	preview, err := st.Preview(req)
	switch {
	case plannerNoticeRefused(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("previewing wait %s's notice: %w", wait.ID, err)
	}
	id := preview.ID
	if _, err := st.Get(id); err == nil {
		if err := recordWaitNotice(sessFront, wait, id); err != nil {
			return false, err
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
	if err := recordWaitNotice(sessFront, wait, id); err != nil {
		return false, err
	}
	_, created, err := st.Propose(req)
	if err != nil {
		return false, fmt.Errorf("proposing wait %s's notice %s: %w", wait.ID, id, err)
	}
	if created {
		lane.Poke(info.ID)
	}
	return true, nil
}

// recordWaitNotice records notice op id as wait's nudge_id, unless it
// already is.
func recordWaitNotice(sessFront *sessionpkg.Store, wait sessionpkg.WaitInfo, id string) error {
	if wait.NudgeID == id {
		return nil
	}
	if err := sessFront.SetWaitNudgeID(wait.ID, id); err != nil {
		return fmt.Errorf("recording wait %s's notice %s: %w", wait.ID, id, err)
	}
	return nil
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

// waitNoticeOp reads wait's lane notice opID from the op journal over
// sessFront's store. found is false when no op has that id: the notice was
// recorded and never proposed (a crash between the two writes), so after a
// rollback the attempt may be the legacy queue's. err is the journal's own
// error, which the caller puts in context.
func waitNoticeOp(sessFront *sessionpkg.Store, opID string) (op opjournal.Op, found bool, err error) {
	st, err := opjournal.NewStore(sessFront.Store(), clock.Real{})
	if err != nil {
		return opjournal.Op{}, false, err
	}
	op, err = st.Get(opID)
	switch {
	case err == nil:
		return op, true, nil
	case errors.Is(err, beads.ErrNotFound):
		return opjournal.Op{}, false, nil
	default:
		return opjournal.Op{}, false, err
	}
}

// finalizeWaitFromNotice ends ready wait from its lane notice opID once the
// notice closes. A closed notice is its attempt's last word, since no pass
// proposes its key again: one that succeeded closes the wait, one that
// failed fails it with the failure's reason, and one a principal canceled,
// or the lane skipped (its conversation closed, as an archived session
// reads, or its context epoch moved on), cancels it, recording why. A
// cancel by gc:wait, the withdrawal of a wait re-armed since, moves the
// wait to its next attempt instead (advancePastWithdrawnNotice). A notice
// this build cannot read (corrupt, or written by a newer build) fails the
// wait as unreadable: the lane quarantines such a row and never delivers
// it, so nothing else would end the wait. A notice still open keeps the
// wait ready. found is false, and nothing is written, when no op has the
// id: the notice was never proposed.
func finalizeWaitFromNotice(sessFront *sessionpkg.Store, wait sessionpkg.WaitInfo, opID string, now time.Time) (done, found bool, err error) {
	op, found, err := waitNoticeOp(sessFront, opID)
	switch {
	case opjournal.Undecodable(err):
		return true, true, sessFront.FailWaitFromNudge(wait.ID, now, opID, "notice unreadable: "+err.Error(), "")
	case err != nil:
		return false, false, fmt.Errorf("reading wait %s's notice %s: %w", wait.ID, opID, err)
	case !found:
		return false, false, nil
	case op.Status != opjournal.StatusClosed:
		return false, true, nil
	}
	arm, reason := op.Outcome.Effective()
	switch arm {
	case opjournal.ArmSucceeded:
		return true, true, sessFront.CloseWaitFromNudge(wait.ID, now, opID, "")
	case opjournal.ArmFailed:
		return true, true, sessFront.FailWaitFromNudge(wait.ID, now, opID, reason, "")
	case opjournal.ArmCanceled:
		if op.Cancel.By == waitNoticePrincipal {
			done, err := advancePastWithdrawnNotice(sessFront, wait, opID, now)
			return done, true, err
		}
		why := "notice canceled by " + op.Cancel.By
		if op.Cancel.Reason != "" {
			why += ": " + op.Cancel.Reason
		}
		return true, true, sessFront.CancelWait(wait.ID, now, why)
	default: // opjournal.ArmSkipped: a decoded op's arm is one of the four.
		return true, true, sessFront.CancelWait(wait.ID, now, "notice skipped: "+reason)
	}
}

// advancePastWithdrawnNotice moves ready wait to its next attempt once its
// lane notice opID closed canceled by gc:wait. gc:wait withdraws a notice
// only from a wait it has ended, so a wait still ready on that notice was
// re-armed while the notice was being delivered, keeping the attempt
// (nextWaitDeliveryAttempt). The delivery did not land, so the attempt is
// void, and the next dispatch proposes the next one. The wait is read again
// first: one that has ended since the pass read it is done, and one already
// on another attempt is left as it is.
func advancePastWithdrawnNotice(sessFront *sessionpkg.Store, wait sessionpkg.WaitInfo, opID string, now time.Time) (done bool, err error) {
	cur, err := sessFront.GetWait(wait.ID)
	switch {
	case err != nil:
		return false, fmt.Errorf("re-reading wait %s past its withdrawn notice %s: %w", wait.ID, opID, err)
	case cur.Status == "closed" || cur.State != waitStateReady:
		return true, nil
	case cur.NudgeID != opID:
		return false, nil
	}
	if err := sessFront.MarkWaitReadyForRedelivery(wait.ID, waitAttemptAfter(cur), now); err != nil {
		return false, fmt.Errorf("moving wait %s past its withdrawn notice %s: %w", wait.ID, opID, err)
	}
	return false, nil
}

// waitNoticeHoldsRetry refuses, saying why, a retry of closed wait while its
// lane notice is still open. The notice may yet be delivered, and the retry
// is a new wait whose notice has a key of its own, so both could be
// delivered: a withdrawal cannot stop a notice already being delivered (gc
// wait cancel, then gc wait ready, while it is), and an expiry withdraws
// none. Once the notice closes, the retry takes the next attempt, which is
// proposed once; gc wait cancel closes a notice not yet being delivered.
func waitNoticeHoldsRetry(sessFront *sessionpkg.Store, wait sessionpkg.WaitInfo) error {
	if !opjournal.IsOpID(wait.NudgeID) {
		return nil
	}
	inFlight, _, err := waitNoticeInFlight(sessFront, wait, wait.NudgeID)
	if err != nil || !inFlight {
		return err
	}
	return fmt.Errorf("wait %s's notice %s is still open and may yet be delivered, so a retry now could deliver the reminder twice; run \"gc wait ready %s\" again once the notice closes, or first \"gc wait cancel %s\" to withdraw a notice not yet being delivered", wait.ID, wait.NudgeID, wait.ID, wait.ID)
}

// waitNoticeInFlight reports whether wait's lane notice opID still holds its
// delivery attempt: the op is open, so its delivery may yet land, and a
// re-arm keeps the attempt, as it does for an open queue shadow. A closed
// notice holds nothing, nor does one this build cannot read, which the lane
// quarantines and never delivers. found is false when no op has the id: the
// notice was never proposed, and the attempt may be the queue's.
func waitNoticeInFlight(sessFront *sessionpkg.Store, wait sessionpkg.WaitInfo, opID string) (inFlight, found bool, err error) {
	op, found, err := waitNoticeOp(sessFront, opID)
	switch {
	case opjournal.Undecodable(err):
		return false, true, nil
	case err != nil:
		return false, false, fmt.Errorf("reading wait %s's notice %s: %w", wait.ID, opID, err)
	}
	return found && op.Status != opjournal.StatusClosed, found, nil
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
// sessions, as gc:wait. A notice already being delivered cannot be stopped:
// its cancel is recorded (opjournal.CancelRequested), the lane's outcome
// says whether the delivery landed, and the withdrawal logs that. A notice
// this build cannot read is left as it is, and logged: the lane quarantines
// such a row and never delivers it, so there is nothing to withdraw.
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
		result, _, err := st.Cancel(id, waitNoticePrincipal, waitNoticeWithdrawReason)
		switch {
		case opjournal.Undecodable(err):
			log.Printf("gc wait: wait notice %s is unreadable, so it is not withdrawn; the lane never delivers a notice it cannot read: %v", id, err)
		case err != nil && !errors.Is(err, beads.ErrNotFound):
			errs = append(errs, fmt.Errorf("withdrawing wait notice %s: %w", id, err))
		case result == opjournal.CancelRequested:
			log.Printf("gc wait: wait notice %s is being delivered; its withdrawal is recorded, and the lane's outcome says whether the delivery landed", id)
		}
	}
	return errors.Join(errs...)
}
