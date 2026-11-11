package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// OperatorKillRequest is one operator kill of a session: `gc session kill`
// and the API session kill both run it through OperatorKill, so either entry
// point leaves the same row, records the same events, and hands the session
// to the controller the same way (#7645). The hooks are the effects only the
// caller's host can perform; a nil hook is skipped.
type OperatorKillRequest struct {
	// SessionID is the resolved session bead ID.
	SessionID string
	// Handle is the worker handle the kill stops through. An open row needs
	// a FencedKillHandle: the kill runs under the kill fence, and its
	// worker.operation event is the handle's.
	Handle Handle
	// Info is the row as the caller read it before the kill, and InfoErr that
	// read's error. An unreadable row is still killed, best-effort: through
	// Handle.Kill with no fence and no circuit-breaker clear.
	Info    sessionpkg.Info
	InfoErr error
	// By is the operator Actor the kill runs as.
	By sessionpkg.Actor
	// Force kills past live background subagents, and Lease may take the
	// runtime lease past a hung holder.
	Force bool

	// LiveSubagents lists the background subagents the kill would destroy.
	// An error fails open: an operator must still be able to kill a damaged
	// or history-less session.
	LiveSubagents func(context.Context) ([]InFlightSubagent, error)
	// Admit runs once the kill passed its refusals (the fence check and the
	// live-subagent refusal), before the lease: `gc session kill` proposes
	// its kill op here (DES-O0API §8.4). An error ends the kill with nothing
	// done and is returned as it is.
	Admit func() error
	// Lease takes the open row's runtime lease (session.KillRuntimeLease);
	// overridden reports a take past a hung holder. Nil leaves the lease to
	// the fenced kill.
	Lease func(ctx context.Context, force bool) (lease *sessionpkg.RuntimeLease, overridden bool, err error)
	// Check runs once the kill holds its runtime lease (an open row's, or
	// the one By lends), before the fence: a recorded kill refuses here a
	// session that moved past the generation its op names. An error ends the
	// kill with nothing killed, the lease released, and is returned as it is.
	Check func() error
	// Settled runs once the kill and the circuit-breaker clear succeeded and
	// the lease is released, before the poke and the events: a caller closes
	// its record of the kill here, so no record write runs under the lease.
	Settled func(OperatorKillResult)
	// ResetCircuitBreaker clears a configured named session's respawn
	// circuit breaker after the kill.
	ResetCircuitBreaker func(identity string) error
	// Poke asks the controller to reconcile the session now (#3812), after
	// the asleep sync and the lease release.
	Poke func() error
	// Recorder receives session.stopped, recorded as EventActor.
	Recorder   events.Recorder
	EventActor string
	// RecordStop records the stop metric.
	RecordStop func()
	// Warn receives the kill's best-effort warnings; nil logs them.
	Warn func(string)
	// Now reads the clock for a refusal's subagent ages; nil reads the wall
	// clock.
	Now func() time.Time
}

// OperatorKillResult is what OperatorKill did.
type OperatorKillResult struct {
	// RuntimeWasGone reports that the session's runtime was already gone.
	RuntimeWasGone bool
	// AlreadyInactive reports a configured named session whose kill failed
	// because its runtime was already gone, counted as done: nothing was
	// killed.
	AlreadyInactive bool
	// LeaseOverridden reports a kill past a hung runtime lease holder.
	LeaseOverridden bool
}

// LiveSubagentsError refuses a kill that would destroy live background
// subagents; the request's Force overrides it. At is when the refusal was
// taken, the moment the subagents' ages are measured at, so Error only
// formats what the error holds.
type LiveSubagentsError struct {
	Subagents []InFlightSubagent
	At        time.Time
}

func (e *LiveSubagentsError) Error() string {
	descriptions := make([]string, 0, len(e.Subagents))
	for _, subagent := range e.Subagents {
		descriptions = append(descriptions, subagent.Summary(e.At))
	}
	return fmt.Sprintf("refusing to destroy %d live background subagent(s); retry with force: %s", len(e.Subagents), strings.Join(descriptions, "; "))
}

// Summary is the subagent as an operator refusal names it:
// "description (agent-id, running 1m2s)".
func (s InFlightSubagent) Summary(now time.Time) string {
	description := strings.TrimSpace(s.Description)
	if description == "" {
		description = "(no description)"
	}
	age := "unknown duration"
	if !s.StartedAt.IsZero() {
		age = now.Sub(s.StartedAt).Round(time.Second).String()
	}
	return fmt.Sprintf("%s (%s, running %s)", description, s.AgentID, age)
}

// sessionKilledPayload is the session.stopped payload of an operator kill:
// the wire form of the API's SessionLifecyclePayload with reason "killed".
type sessionKilledPayload struct {
	SessionID string `json:"session_id"`
	Reason    string `json:"reason,omitempty"`
}

// OperatorKill is the operator kill of a session, `gc session kill`'s
// protocol for every entry point:
//
//   - an open row needs a handle that can fence the kill;
//   - unless forced, a kill whose runtime is not already gone refuses to
//     destroy live background subagents (LiveSubagentsError);
//   - the caller admits the kill (Admit);
//   - the runtime lease is held from before the caller's last check (Check)
//     through the fence write, the stop and the asleep sync, and the kill
//     runs under the kill fence (session.Manager.KillFencedReport), leaving
//     the row asleep/killed (#3629);
//   - a configured named session's circuit breaker is cleared, and its kill
//     counts as done when the runtime was already gone;
//   - after the lease is released the caller settles the kill (Settled), the
//     controller is poked (#3812), and session.stopped and the stop metric
//     are recorded.
//
// A kill error is returned as it is, before any of the after-effects.
func OperatorKill(ctx context.Context, req OperatorKillRequest) (OperatorKillResult, error) {
	var res OperatorKillResult
	warn := req.Warn
	if warn == nil {
		warn = func(msg string) { log.Printf("session kill: %s", msg) }
	}
	open := req.InfoErr == nil && !req.Info.Closed
	fenced, canFence := req.Handle.(FencedKillHandle)
	if open && !canFence {
		return res, fmt.Errorf("session %s: handle %T cannot fence the kill", req.SessionID, req.Handle)
	}
	// Taken here, before the lease, this verdict gates only the
	// live-subagent refusal; the fenced kill decides it again on a fresh
	// read under the lease, and that decision is the one acted on.
	runtimeGone := false
	if req.InfoErr == nil && canFence {
		runtimeGone, _ = fenced.KillRuntimeGone(ctx)
	}
	if !req.Force && !runtimeGone && req.LiveSubagents != nil {
		if live, err := req.LiveSubagents(ctx); err == nil && len(live) > 0 {
			now := time.Now
			if req.Now != nil {
				now = req.Now
			}
			return res, &LiveSubagentsError{Subagents: live, At: now()}
		}
	}
	if req.Admit != nil {
		if err := req.Admit(); err != nil {
			return res, err
		}
	}

	by := req.By
	releaseLease := func() {}
	if open && req.Lease != nil {
		lease, overridden, err := req.Lease(ctx, req.Force)
		if err != nil {
			return res, err
		}
		releaseLease, res.LeaseOverridden = lease.Release, overridden
		defer releaseLease()
		by.Lease = lease
	}
	if req.Check != nil {
		if err := req.Check(); err != nil {
			return res, err
		}
	}
	var killErr error
	if open {
		report, err := fenced.KillFenced(ctx, by, warn)
		killErr, runtimeGone = err, report.RuntimeWasGone
	} else {
		killErr = req.Handle.Kill(ctx, by)
	}
	res.RuntimeWasGone = runtimeGone
	identity := ""
	if req.InfoErr == nil {
		identity = sessionpkg.NamedSessionIdentityInfo(req.Info)
	}
	if killErr != nil && (identity == "" || !runtimeGone) {
		return res, killErr
	}

	if req.InfoErr != nil {
		warn(fmt.Sprintf("loading session %s for circuit breaker clear: %v", req.SessionID, req.InfoErr))
	} else if identity != "" && req.ResetCircuitBreaker != nil {
		if err := req.ResetCircuitBreaker(identity); err != nil {
			warn(fmt.Sprintf("clearing session circuit breaker for %q: %v", identity, err))
			if killErr != nil {
				return res, killErr
			}
		}
	}
	if killErr != nil {
		res.AlreadyInactive = true
		warn(fmt.Sprintf("session %s runtime was already inactive; cleared named-session circuit breaker", req.SessionID))
	}

	// The lease ends before the poke, so the pass the poke wakes never finds
	// the name busy. Best-effort: a poke failure (no controller running) is
	// only a warning.
	releaseLease()
	if req.Settled != nil {
		req.Settled(res)
	}
	if req.Poke != nil {
		if err := req.Poke(); err != nil {
			warn(fmt.Sprintf("poke failed: %v", err))
		}
	}
	if req.Recorder != nil {
		message := "killed"
		if res.LeaseOverridden {
			message = "killed past a hung runtime lease holder (--force)"
		}
		payload, _ := json.Marshal(sessionKilledPayload{SessionID: req.SessionID, Reason: "killed"})
		req.Recorder.Record(events.Event{
			Type:    events.SessionStopped,
			Actor:   req.EventActor,
			Subject: req.SessionID,
			Message: message,
			Payload: payload,
		})
	}
	if req.RecordStop != nil {
		req.RecordStop()
	}
	return res, nil
}
