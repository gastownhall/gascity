package main

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// nudgeOutcomeUndeliverable is the outcome of a session nudge whose target
// session can never take it: nothing is left pending for it.
const nudgeOutcomeUndeliverable = "undeliverable"

// nudgeUndeliverableError is a session nudge refused because its target
// session is gone for good (retired), so nothing was queued.
type nudgeUndeliverableError struct {
	SessionID string
	State     string
}

// Error names the session and the state that makes the nudge undeliverable.
func (e *nudgeUndeliverableError) Error() string {
	return fmt.Sprintf("session %s is %s; the nudge is undeliverable", e.SessionID, e.State)
}

// Unwrap reports the refusal as a closed session.
func (e *nudgeUndeliverableError) Unwrap() error { return session.ErrSessionClosed }

// nudgeFenceRetired reports whether the session row a nudge is fenced to (or
// targets) is retired for good: session.LifecycleRetired, or a closed named
// row whose identity cfg no longer declares, which the reconciler will not
// reopen. Retirement is absorbing, so the verdict cannot race a resume.
func nudgeFenceRetired(cfg *config.City, persisted session.PersistedResponse) bool {
	if session.LifecycleRetired(persisted.Status, persisted.Metadata) {
		return true
	}
	if strings.TrimSpace(persisted.Status) != "closed" {
		return false
	}
	identity := strings.TrimSpace(persisted.Metadata[session.NamedSessionIdentityMetadata])
	return identity != "" && config.FindNamedSession(cfg, identity) == nil
}

// refuseNudgeToRetiredSession reads target's session row once through
// sessions and refuses the nudge as undeliverable when that row is retired
// (nudgeFenceRetired), before anything is queued or typed. A target without a
// session row or a store is not judged. A read failure is returned as an
// error, never read as retired.
func refuseNudgeToRetiredSession(target nudgeTarget, sessions *session.Store) error {
	if sessions == nil || target.sessionID == "" {
		return nil
	}
	_, persisted, err := sessions.GetPersistedResponse(target.sessionID)
	if err != nil {
		return fmt.Errorf("checking session %q before nudge: %w", target.sessionID, err)
	}
	if !nudgeFenceRetired(target.cfg, persisted) {
		return nil
	}
	return &cliMessageRefusal{why: cliOpRefusedSessionClosed, err: &nudgeUndeliverableError{SessionID: target.sessionID, State: string(session.BaseStateClosed)}}
}

// writeUndeliverableSessionNudge reports an undeliverable nudge and exits 1:
// under --json one sessionNudgeJSON line (ok:false, queued:false,
// outcome:"undeliverable", the refusal's reason and the session's state),
// which stands in for the op's failure envelope; otherwise one stderr line.
func writeUndeliverableSessionNudge(target nudgeTarget, mode nudgeDeliveryMode, jsonOutput bool, err error, op *cliOp, stdout, stderr io.Writer) (int, bool) {
	var undeliverable *nudgeUndeliverableError
	if !errors.As(err, &undeliverable) {
		return 0, false
	}
	reason := cliOpRefusedSessionClosed
	var refusal *cliMessageRefusal
	if errors.As(err, &refusal) {
		reason = refusal.why
	}
	fmt.Fprintf(stderr, "gc session nudge: %s: session %s is %s (%s); nothing was queued\n", nudgeOutcomeUndeliverable, undeliverable.SessionID, undeliverable.State, target.agentKey()) //nolint:errcheck // best-effort stderr
	if !jsonOutput {
		return 1, true
	}
	op.markResultWritten()
	result := sessionNudgeJSON{
		SchemaVersion: "1",
		OK:            false,
		Target:        target.agentKey(),
		SessionID:     target.sessionID,
		SessionName:   target.sessionName,
		Delivery:      string(mode),
		Queued:        false,
		Outcome:       nudgeOutcomeUndeliverable,
		Reason:        reason,
		SessionState:  undeliverable.State,
	}
	result.OpID, result.Key, result.Replayed = op.jsonFields()
	if writeErr := writeCLIJSONLine(stdout, result); writeErr != nil {
		fmt.Fprintf(stderr, "gc session nudge: writing JSON result: %v\n", writeErr) //nolint:errcheck // best-effort stderr
	}
	return 1, true
}
