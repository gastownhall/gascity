package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// The managed-hook deadline.
//
// `gc hook run` runs a hook command in a child and discards the child's output
// when the child overruns the timeout. A child that injects auto-handoff mail
// and then archives it can be reported as timed out AFTER it archived: the
// agent never sees the handoff, and it is already marked read and closed. So
// gc hook run hands the child its deadline, and a child skips a destructive
// post-injection step when too little time is left to finish it and exit. The
// skipped mail stays unread and the next hook delivers it again — the
// at-least-once posture the nudge queue already has.

// hookDeadlineEnv names the absolute time (RFC 3339, UTC) by which a child of
// gc hook run must have exited for its output to be kept.
const hookDeadlineEnv = "GC_HOOK_DEADLINE"

// hookDeadlineArchiveMargin is the time an injecting hook must have left to
// archive what it injected and exit: a mark-read update, a close, and the
// process exit, against a slow remote ledger.
const hookDeadlineArchiveMargin = 3 * time.Second

// hookDeadlineEnvValue renders deadline for hookDeadlineEnv.
func hookDeadlineEnvValue(deadline time.Time) string {
	return deadline.UTC().Format(time.RFC3339Nano)
}

// hookDeadlineLeaves reports whether at least margin remains before the
// deadline gc hook run handed this process. Without a deadline there is no
// limit. A deadline that does not parse is reported as an error, with no time
// assumed left: the caller cannot know whether its output will be kept.
func hookDeadlineLeaves(margin time.Duration, now time.Time) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(hookDeadlineEnv))
	if raw == "" {
		return true, nil
	}
	deadline, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return false, fmt.Errorf("parsing %s=%q: %w", hookDeadlineEnv, raw, err)
	}
	return deadline.Sub(now) >= margin, nil
}

// hookDeadlineAllowsArchive reports whether an injecting hook has time to
// archive what it injected. When it does not, it says so on stderr under
// cmdName, so a skipped archive is visible rather than silent.
func hookDeadlineAllowsArchive(cmdName string, stderr io.Writer) bool {
	ok, err := hookDeadlineLeaves(hookDeadlineArchiveMargin, time.Now())
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v; leaving injected auto-handoff mail unarchived\n", cmdName, err) //nolint:errcheck // best-effort stderr
		return false
	}
	if !ok {
		fmt.Fprintf(stderr, "%s: hook deadline too close to archive injected auto-handoff mail; it will be delivered again\n", cmdName) //nolint:errcheck // best-effort stderr
		return false
	}
	return true
}
