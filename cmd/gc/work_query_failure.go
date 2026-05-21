package main

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/events"
)

// classifyWorkQueryKill inspects a work-query runner error and reports
// whether the subprocess was killed by an external signal or aborted by
// the runner-imposed timeout, along with a short human-readable reason.
//
// A killed or timed-out work query strands the session: the startup
// nudge produces no output, the pane dies, and nothing names the cause
// (issue #1496). Ordinary command failures (non-zero exit with output,
// bad config) are NOT classified as kills — those already surface on the
// caller's stderr path and do not warrant a lifecycle event.
func classifyWorkQueryKill(err error) (reason string, killed bool) {
	if err == nil {
		return "", false
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "signal: killed"):
		return "work query killed (signal: killed)", true
	case strings.Contains(msg, "signal: terminated"):
		return "work query terminated (signal: terminated)", true
	case strings.Contains(msg, "exit status 137"):
		return "work query killed (exit status 137 / SIGKILL)", true
	case strings.Contains(msg, "exit status 143"):
		return "work query terminated (exit status 143 / SIGTERM)", true
	case strings.Contains(msg, "timed out after"):
		return "work query timed out", true
	default:
		return "", false
	}
}

// emitWorkQueryFailure records a SessionWorkQueryFailed event when a
// work-query subprocess was killed or timed out, giving the reconciler a
// named cause to escalate on instead of letting the session die silently
// into unknown state (issue #1496, companion #1497). Best-effort: a nil
// recorder is treated as a discard. Returns true when the failure was recorded,
// false for ordinary errors or when no current session ID is available.
func emitWorkQueryFailure(rec events.Recorder, sessionID, template, command string, err error) bool {
	reason, killed := classifyWorkQueryKill(err)
	if !killed {
		return false
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return false
	}
	if rec == nil {
		rec = events.Discard
	}
	template = strings.TrimSpace(template)
	subject := template
	if subject == "" {
		subject = sessionID
	}
	rec.Record(events.Event{
		Type:    events.SessionWorkQueryFailed,
		Actor:   eventActor(),
		Subject: subject,
		Message: fmt.Sprintf("%s while running %q: %v", reason, command, err),
		Payload: api.SessionLifecyclePayloadJSON(sessionID, template, reason),
	})
	return true
}
