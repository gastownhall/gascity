package main

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// classifyGCNoAPI decides whether a GC_NO_API env value disables API routing.
// Truthy values (1/true/yes, case-insensitive, surrounding whitespace stripped)
// disable routing. Empty/unset and well-known falsy values (0/false/no) leave
// routing enabled. Anything else is treated as unset but returns a warning
// string so the caller can surface the unknown value to the operator (fail-open).
func classifyGCNoAPI(val string) (disabled bool, warn string) {
	trimmed := strings.TrimSpace(val)
	if trimmed == "" {
		return false, ""
	}
	switch strings.ToLower(trimmed) {
	case "1", "true", "yes":
		return true, ""
	case "0", "false", "no":
		return false, ""
	}
	return false, fmt.Sprintf("GC_NO_API=%q is not recognized; treating as unset (valid: 1/true/yes to disable, empty to enable)", val)
}

// routeLogEnabled reports whether route=... lines should be emitted on stderr.
// Routing logs are opt-in via GC_DEBUG so the default CLI experience stays quiet;
// per-file test suites and operators debugging fallback behavior enable it.
func routeLogEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GC_DEBUG"))) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// logRoute emits a single route audit line to the given writer when GC_DEBUG
// is truthy. Each line carries cmd=<cmd> route=<route> reason=<reason>,
// followed by any extras as key=value pairs. A route=api line typically
// omits reason; a route=fallback line always includes it.
//
// Callers pass their command-scoped stderr so CLI test harnesses observe
// the audit trail through the buffer they already assert on, instead of
// racing against the process-wide os.Stderr.
func logRoute(w io.Writer, cmd, route, reason string, extra ...string) {
	if !routeLogEnabled() {
		return
	}
	logRouteTo(w, cmd, route, reason, extra...)
}

// logRouteTo writes the route audit line to the given writer unconditionally.
// The public wrapper (logRoute) gates on GC_DEBUG; this form is exported to
// tests so they can assert formatting without toggling env state.
//
// Extras are interpreted as alternating key/value tokens and rendered as
// key=value. A trailing unpaired token is emitted verbatim.
func logRouteTo(w io.Writer, cmd, route, reason string, extra ...string) {
	var b strings.Builder
	b.WriteString("cmd=")
	b.WriteString(cmd)
	b.WriteString(" route=")
	b.WriteString(route)
	if reason != "" {
		b.WriteString(" reason=")
		b.WriteString(reason)
	}
	for i := 0; i < len(extra); i += 2 {
		b.WriteByte(' ')
		if i+1 < len(extra) {
			b.WriteString(extra[i])
			b.WriteByte('=')
			b.WriteString(extra[i+1])
		} else {
			b.WriteString(extra[i])
		}
	}
	b.WriteByte('\n')
	_, _ = fmt.Fprint(w, b.String())
}
