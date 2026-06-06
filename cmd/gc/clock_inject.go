package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// emitClockInject writes a one-line current-time stamp (operator-local + UTC +
// epoch) as UserPromptSubmit hook context. It is called at the top of
// "gc nudge drain --inject" — which fires unconditionally on every prompt — so
// agents always have a live clock in context, without spawning an extra hook
// subprocess per turn.
//
// Rationale: agents reason heavily over UTC timestamps (supervisor logs,
// dolt_log, events.jsonl, mail headers) but otherwise have no running clock,
// which leads to mis-dated cause/effect and operator-TZ-vs-server-UTC confusion.
//
// Disable with GC_INJECT_CLOCK=0 (or "false"/"off"). Override the local zone
// with GC_OPERATOR_TZ (an IANA name, e.g. "America/Los_Angeles"); otherwise the
// host zone (time.Local, which honors $TZ) is used — useful when the server
// runs UTC but the operator thinks in their own timezone.
func emitClockInject(hookFormat string, stdout io.Writer) {
	line := clockInjectLine()
	if line == "" {
		return
	}
	_ = writeProviderHookContextForEvent(stdout, hookFormat, "UserPromptSubmit", line)
}

// clockInjectLine returns the current-time stamp that emitClockInject would
// write, or "" when clock injection is disabled via GC_INJECT_CLOCK
// (0/false/off). Callers that already emit a UserPromptSubmit hook context
// (e.g. the nudge inject path) prepend this so the clock and the nudge ride in
// a single provider-formatted payload, keeping JSON formats one valid document.
func clockInjectLine() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GC_INJECT_CLOCK"))) {
	case "0", "false", "off":
		return ""
	}
	return formatClockLine(time.Now())
}

// formatClockLine renders, e.g.:
//
//	Current time: Wed 2026-06-03 2:23PM PDT / 2026-06-03 21:23Z UTC (epoch 1780521833)
func formatClockLine(now time.Time) string {
	loc := time.Local
	if tz := strings.TrimSpace(os.Getenv("GC_OPERATOR_TZ")); tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			loc = l
		}
	}
	return fmt.Sprintf(
		"Current time: %s / %sZ UTC (epoch %d)\n",
		now.In(loc).Format("Mon 2006-01-02 3:04PM MST"),
		now.UTC().Format("2006-01-02 15:04"),
		now.Unix(),
	)
}
