package herdr

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/runtime"
)

// agentStartOps are the herdr operations resolveAgentNameTaken needs to recover
// from an agent_name_taken rejection. They are injected as closures so the
// recovery decision is unit-testable without a live herdr server.
type agentStartOps struct {
	// getAgent fetches the agent currently holding the contested name.
	getAgent func() (agentInfo, bool, error)
	// paneAlive reports whether the holder's pane still runs the agent process.
	// A non-nil error means the probe could not tell either way.
	paneAlive func(paneID string) (bool, error)
	// closePane reaps a stale holder pane.
	closePane func(paneID string) error
	// retryStart re-issues the original agent start after a stale holder is reaped.
	retryStart func() (agentInfo, error)
}

// resolveAgentNameTaken recovers from herdr's agent_name_taken rejection, which
// fires when gc re-issues `agent start` for a name herdr still holds. herdr can
// report a live agent's pane as status=Unknown, so gc's liveness deems it dead
// and tries to recreate it; without recovery gc then spawns a fresh tab and
// retries indefinitely — the pane/PTY/process storm.
//
// startInfo/startErr are the original startAgent result. On success, or on any
// error other than agent_name_taken, the input is returned unchanged with
// adopted=false. On agent_name_taken it inspects the holder: if the holder's
// process is alive it is adopted (returned as-is with adopted=true, no new pane,
// no retry) so the caller can skip re-priming a running agent; if the holder is
// a stale pane it is reaped and the start is retried exactly once (adopted=false,
// a fresh agent). If the holder cannot be inspected, nothing is reaped or
// retried: the original error is surfaced rather than guessing (see
// holderUninspectable).
func resolveAgentNameTaken(startInfo agentInfo, startErr error, ops agentStartOps) (info agentInfo, adopted bool, err error) {
	if startErr == nil {
		return startInfo, false, nil
	}
	if herdrVerdictCode(startErr) != "agent_name_taken" {
		return agentInfo{}, false, startErr
	}
	existing, ok, gerr := ops.getAgent()
	if gerr != nil || !ok {
		return agentInfo{}, false, holderUninspectable(startErr, gerr)
	}
	alive, perr := ops.paneAlive(existing.PaneID)
	if perr != nil {
		return agentInfo{}, false, holderUninspectable(startErr, perr)
	}
	if alive {
		return existing, true, nil // adopt the live holder; no reap, no retry
	}
	_ = ops.closePane(existing.PaneID) // reap the stale holder (best effort)
	fresh, rerr := ops.retryStart()    // bounded: exactly one retry
	return fresh, false, rerr
}

// holderUninspectable is resolveAgentNameTaken's error when the name's holder
// could not be found or probed: startErr, joined by the inspection's own error
// when the inspection failed. An inspection that went unanswered (see
// unanswered) proves nothing about the holder, which may well be live, so the
// error then also wraps runtime.ErrRuntimeUnavailable: Start defers, as its
// existence check does, rather than failing.
func holderUninspectable(startErr, inspectErr error) error {
	switch {
	case inspectErr == nil:
		return startErr
	case unanswered(inspectErr):
		return fmt.Errorf("%w: %w; inspecting the name's holder: %w", runtime.ErrRuntimeUnavailable, startErr, inspectErr)
	default:
		return fmt.Errorf("%w; inspecting the name's holder: %w", startErr, inspectErr)
	}
}
