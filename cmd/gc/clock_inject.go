package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// clockInjectLine returns a one-line current-time stamp (operator-local + UTC
// + epoch) for UserPromptSubmit hook context, or "" when clock injection is
// disabled via GC_INJECT_CLOCK (0/false/off). It is folded into the inject
// prefix of "gc nudge drain --inject" — which fires unconditionally on every
// prompt — so agents always have a live clock in context without an extra
// hook subprocess per turn; the prefix rides in a single provider-formatted
// payload so JSON formats stay one valid document (see cmd_nudge.go and
// context_inject.go, the context-pressure sibling of this file).
//
// Rationale: agents reason heavily over UTC timestamps (supervisor logs,
// dolt_log, events.jsonl, mail headers) but otherwise have no running clock,
// which leads to mis-dated cause/effect and operator-TZ-vs-server-UTC
// confusion. Override the local zone with GC_OPERATOR_TZ (an IANA name);
// otherwise the host zone (time.Local, which honors $TZ) is used.
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
