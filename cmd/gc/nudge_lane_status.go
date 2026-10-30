package main

// gc nudge status and gc nudge drop over conversation-lane ops (OJ-21 21a,
// DES-OJ21 §9). A message, notice or wait nudge the lane delivers is an op,
// not a queue item, so the two commands read and cancel ops beside the queue
// while both exist. An op shows in the command's schema-v1 item shape: its id
// is the op id, agent the target's queue key, source the producer's
// principal, message the payload, deliver_after its not-before (else its
// creation), expires_at its start-by, attempts its attempt count, and
// claimed_at and lease_until its lease's. A submitted or accepted op is
// pending, an executing or settling one in flight, and a lane op closed
// failed or skipped within the last hour is dead, its dead_at the close.

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/opjournal"
)

const (
	// laneNudgeDeadWindow is how far back gc nudge status lists closed
	// lane ops as dead, as the queue keeps dead letters about that long.
	laneNudgeDeadWindow = time.Hour
	// laneNudgeHistoryRows caps the op rows gc nudge status reads for a
	// session's dead lane ops (opjournal.Store.RecentClosed).
	laneNudgeHistoryRows = 50
	// laneNudgeDropPrincipal is who gc nudge drop cancels a lane op as.
	laneNudgeDropPrincipal = "operator"
)

// laneNudgeStore is the op store gc nudge status and drop read lane ops
// from, and its close.
type laneNudgeStore struct {
	st    *opjournal.Store
	close func()
}

// openLaneNudgeStore opens the op store of the city's sessions class. ok is
// false, with no error and nothing to close, when the sessions store cannot
// host conversation lanes (laneStoreCapability) or has no id prefix for op
// ids, so it holds no lane op.
func openLaneNudgeStore(cityPath string, cfg *config.City) (s laneNudgeStore, ok bool, err error) {
	store, err := openCityStoreAtWithConfig(cityPath, cfg)
	if err != nil {
		return laneNudgeStore{}, false, fmt.Errorf("opening the city store: %w", err)
	}
	closeStore := func() { _ = closeBeadStoreHandle(store) }
	sessions := cliSessionStore(store, cfg, cityPath)
	if capErr := laneStoreCapability(sessions); capErr != nil {
		closeStore()
		if errors.Is(capErr, errLaneStoreMissing) || errors.Is(capErr, errLaneStoreIncapable) {
			return laneNudgeStore{}, false, nil
		}
		return laneNudgeStore{}, false, capErr
	}
	st, err := opjournal.NewStore(sessionFrontDoor(sessions).Store(), cliOpClock)
	switch {
	case errors.Is(err, opjournal.ErrNoNamespace):
		// A store with no id prefix never held an op: the journal mints
		// every op id under it.
		closeStore()
		return laneNudgeStore{}, false, nil
	case err != nil:
		closeStore()
		return laneNudgeStore{}, false, fmt.Errorf("opening the op journal: %w", err)
	}
	return laneNudgeStore{st: st, close: closeStore}, true, nil
}

// laneNudgeItems lists target's lane ops as gc nudge status items at now:
// its open lane ops, pending or in flight, and those closed failed or
// skipped within laneNudgeDeadWindow, dead. A target with no session id has
// none.
func laneNudgeItems(st *opjournal.Store, target nudgeTarget, now time.Time) (pending, inFlight, dead []queuedNudge, err error) {
	if target.sessionID == "" {
		return nil, nil, nil, nil
	}
	open, err := st.ListOpen(target.sessionID)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, op := range open {
		if op.Executor != opjournal.ExecutorLane {
			continue
		}
		item := laneNudgeItem(op, target)
		switch op.Status {
		case opjournal.StatusExecuting, opjournal.StatusSettling:
			inFlight = append(inFlight, item)
		default:
			pending = append(pending, item)
		}
	}
	closed, err := st.RecentClosed(target.sessionID, laneNudgeHistoryRows)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, op := range closed {
		arm, _ := op.Outcome.Effective()
		at := op.ClosedAt()
		if (arm != opjournal.ArmFailed && arm != opjournal.ArmSkipped) || at.Before(now.Add(-laneNudgeDeadWindow)) {
			continue
		}
		item := laneNudgeItem(op, target)
		item.DeadAt = at
		item.LastError = laneNudgeOutcomeText(op.Outcome)
		dead = append(dead, item)
	}
	return pending, inFlight, dead, nil
}

// laneNudgeItem is op in gc nudge status's item shape.
func laneNudgeItem(op opjournal.Op, target nudgeTarget) queuedNudge {
	deliverAfter := op.NotBefore
	if deliverAfter.IsZero() {
		deliverAfter = op.CreatedAt
	}
	item := queuedNudge{
		ID:           op.ID,
		Agent:        target.agentKey(),
		SessionID:    op.Conversation,
		Source:       op.Producer.Principal,
		Message:      op.Payload,
		CreatedAt:    op.CreatedAt,
		DeliverAfter: deliverAfter,
		ExpiresAt:    op.StartBy,
		Attempts:     op.Attempt,
		LastError:    op.LastError,
		LeaseUntil:   op.Lease.Deadline,
	}
	if !op.Lease.IsZero() {
		item.ClaimedAt = laneNudgeTakenAt(op)
	}
	return item
}

// laneNudgeTakenAt is when op's current attempt began: its last transition
// to executing.
func laneNudgeTakenAt(op opjournal.Op) time.Time {
	for i := len(op.Transitions) - 1; i >= 0; i-- {
		if op.Transitions[i].Status == opjournal.StatusExecuting {
			return op.Transitions[i].At
		}
	}
	return time.Time{}
}

// laneNudgeOutcomeText is a closed op's outcome as gc nudge status's dead
// reason: its arm and reason.
func laneNudgeOutcomeText(o opjournal.Outcome) string {
	arm, reason := o.Effective()
	if reason == "" {
		return string(arm)
	}
	return string(arm) + ": " + reason
}

// laneNudgeDrop cancels lane op id as gc nudge drop does: an op that had not
// started closes canceled (dropped), and an executing one records the
// cancel (cancel_requested), both ok. found is false when id names no lane
// op, which leaves the id to the queue's answer.
func laneNudgeDrop(st *opjournal.Store, id string) (res nudgeDropResult, found bool, err error) {
	op, err := st.Get(id)
	switch {
	case errors.Is(err, beads.ErrNotFound), errors.Is(err, opjournal.ErrCorruptOp):
		return nudgeDropResult{}, false, nil
	case err != nil:
		return nudgeDropResult{}, false, err
	case op.Executor != opjournal.ExecutorLane:
		return nudgeDropResult{}, false, nil
	}
	result, _, err := st.Cancel(id, laneNudgeDropPrincipal, "gc nudge drop")
	if err != nil {
		return nudgeDropResult{}, true, fmt.Errorf("canceling op %s: %w", id, err)
	}
	switch result {
	case opjournal.CancelClosed:
		return nudgeDropResult{ID: id, OK: true, Outcome: "dropped"}, true, nil
	case opjournal.CancelRequested:
		return nudgeDropResult{ID: id, OK: true, Outcome: "cancel_requested"}, true, nil
	default:
		return nudgeDropResult{ID: id, OK: false, Error: "already closed"}, true, nil
	}
}

// laneNudgeStatusItems is laneNudgeItems over the city's op store for gc
// nudge status. A store that does not open, or a read that fails, is warned
// of on stderr and lists no lane op: the queue's items still print.
func laneNudgeStatusItems(target nudgeTarget, now time.Time, stderr io.Writer) (pending, inFlight, dead []queuedNudge) {
	if target.sessionID == "" {
		return nil, nil, nil
	}
	ls, ok, err := openLaneNudgeStore(target.cityPath, target.cfg)
	if err != nil {
		fmt.Fprintf(stderr, "gc nudge status: warning: conversation-lane ops not listed: %v\n", err) //nolint:errcheck // best-effort stderr
		return nil, nil, nil
	}
	if !ok {
		return nil, nil, nil
	}
	defer ls.close()
	pending, inFlight, dead, err = laneNudgeItems(ls.st, target, now)
	if err != nil {
		fmt.Fprintf(stderr, "gc nudge status: warning: conversation-lane ops not listed: %v\n", err) //nolint:errcheck // best-effort stderr
		return nil, nil, nil
	}
	return pending, inFlight, dead
}

// dropLaneNudges cancels, as gc nudge drop, each of ids that is no queued
// nudge (statuses) and is shaped like an op id, and returns the result of
// each one that names a lane op. It opens the city's op store only when
// such an id is given; a store that cannot host lanes holds none.
func dropLaneNudges(cityPath string, ids []string, statuses map[string]nudgeDropStatus) (map[string]nudgeDropResult, error) {
	var candidates []string
	for _, id := range ids {
		if _, queued := statuses[id]; !queued && opjournal.IsOpID(id) {
			candidates = append(candidates, id)
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		return nil, fmt.Errorf("loading the city config: %w", err)
	}
	ls, ok, err := openLaneNudgeStore(cityPath, cfg)
	if err != nil || !ok {
		return nil, err
	}
	defer ls.close()
	out := make(map[string]nudgeDropResult, len(candidates))
	for _, id := range candidates {
		res, found, err := laneNudgeDrop(ls.st, id)
		if err != nil {
			return nil, err
		}
		if found {
			out[id] = res
		}
	}
	return out, nil
}
