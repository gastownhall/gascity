package main

import (
	"fmt"
	"io"
	"strconv"
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
		if reason, ok := classifySignalExitStatus(msg); ok {
			return reason, true
		}
		return "", false
	}
}

func classifySignalExitStatus(msg string) (string, bool) {
	const marker = "exit status "
	idx := strings.LastIndex(msg, marker)
	if idx < 0 {
		return "", false
	}
	fields := strings.Fields(msg[idx+len(marker):])
	if len(fields) == 0 {
		return "", false
	}
	codeText := strings.Trim(fields[0], ".,:;)")
	code, err := strconv.Atoi(codeText)
	if err != nil {
		return "", false
	}
	if code < 129 || code > 159 {
		return "", false
	}
	return fmt.Sprintf("work query terminated by signal (exit status %d)", code), true
}

// emitCityWorkQueryFailure records a work-query failure against the city event
// log and closes file-backed recorders after the best-effort write.
func emitCityWorkQueryFailure(cityPath string, stderr io.Writer, sessionID, template, command string, err error) {
	rec := openCityRecorderAt(cityPath, stderr)
	if closer, ok := rec.(interface{ Close() error }); ok {
		defer closer.Close() //nolint:errcheck // best-effort event recorder cleanup
	}
	emitWorkQueryFailure(rec, sessionID, template, command, err)
}

// emitWorkQueryFailure records a SessionWorkQueryFailed event when a
// work-query subprocess was killed or timed out, giving the reconciler a
// named cause to escalate on instead of letting the session die silently
// into unknown state (issue #1496, companion #1497). Best-effort: a nil
// recorder is treated as a discard. Returns true when the failure was recorded,
// false for ordinary errors or when no current session ID is available.
func emitWorkQueryFailure(rec events.Recorder, sessionID, template, _ string, err error) bool {
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
		Message: reason,
		Payload: api.SessionLifecyclePayloadJSON(sessionID, template, reason),
	})
	return true
}
