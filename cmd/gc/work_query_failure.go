package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/events"
)

// classifyWorkQueryFailure inspects a work-query runner error and reports the
// reason to record, or recordable=false when there is nothing to record.
//
// EVERY non-nil error is recordable. A work query that fails is a failed READ,
// and the defect this widening closes is that a failed read and an idle store
// were indistinguishable to everything downstream: `gc ready` aborts the whole
// federated read when any leg errors (ready_federation.go), the generated query
// propagates that exit, and the hook exited 1 with nothing on the bus — so a
// storage refusal, a contended SQLite leg or a frontier refusal at the spawn
// instant looked exactly like a quiet city. Kills and timeouts keep their
// specific reasons (issue #1496, companion #1497); ordinary non-zero exits, which
// used to be classified un-recordable because "those already surface on the
// caller's stderr path", now record too — nothing supervises a worker's stderr.
func classifyWorkQueryFailure(err error) (reason string, recordable bool) {
	if err == nil {
		return "", false
	}
	if reason, killed := classifyWorkQueryKill(err); killed {
		return reason, true
	}
	return "work query failed: " + workQueryFailureDetail(err), true
}

// workQueryFailureDetail renders a bounded single-line form of a query error for
// the event message. The raw error can carry a whole command line and captured
// stderr; the event is a signal, not a log sink, so it is trimmed to its first
// line and capped.
func workQueryFailureDetail(err error) string {
	msg := strings.TrimSpace(err.Error())
	if idx := strings.IndexByte(msg, '\n'); idx >= 0 {
		msg = strings.TrimSpace(msg[:idx])
	}
	const maxDetail = 200
	if len(msg) > maxDetail {
		msg = msg[:maxDetail] + "…"
	}
	return msg
}

// classifyWorkQueryKill inspects a work-query runner error and reports
// whether the subprocess was killed by an external signal or aborted by
// the runner-imposed timeout, along with a short human-readable reason.
//
// A killed or timed-out work query strands the session: the startup
// nudge produces no output, the pane dies, and nothing names the cause
// (issue #1496). It is kept separate from classifyWorkQueryFailure because the
// kill/timeout REASONS are load-bearing for the reconciler's escalation, while
// the recordability decision is now simply "did the read fail".
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

// emitWorkQueryFailure records a SessionWorkQueryFailed event when a work query
// FAILED — killed, timed out, or exited non-zero — giving the reconciler a named
// cause to escalate on instead of letting the session die silently into unknown
// state (issue #1496, companion #1497). Best-effort: a nil recorder is treated as
// a discard. Returns true when the failure was recorded, false when there was no
// error or no current session ID.
//
// The payload shape is unchanged; only the reason text distinguishes the
// widened ordinary-failure case, so no consumer needs a schema change to read it.
func emitWorkQueryFailure(rec events.Recorder, sessionID, template, _ string, err error) bool {
	reason, recordable := classifyWorkQueryFailure(err)
	if !recordable {
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
