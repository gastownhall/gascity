package main

import (
	"context"
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
// skipped mail stays unread and the next hook delivers it again. An injecting
// nudge drain is gated the same way: its ack of the nudges it wrote is the
// destructive step, so with too little time left it releases its claims
// instead, and the next drain delivers them again. The margin is a heuristic:
// it makes a lost delivery unlikely, not impossible — a destructive step that
// starts with the margin left and then runs past the deadline still loses the
// output it was retiring.

// hookDeadlineEnv names the absolute time (RFC 3339, UTC) by which a child of
// gc hook run must have exited for its output to be kept.
const hookDeadlineEnv = "GC_HOOK_DEADLINE"

// hookDeadlineArchiveMarginMax caps the time an injecting hook must have left
// to retire what it injected and exit: the archive's or the nudge ack's
// writes and the process exit, against a slow remote ledger.
const hookDeadlineArchiveMarginMax = 3 * time.Second

// hookProcessStart is when this process started, the start of the time gc
// hook run gave it.
var hookProcessStart = time.Now()

// hookDeadlineEnvValue renders deadline for hookDeadlineEnv.
func hookDeadlineEnvValue(deadline time.Time) string {
	return deadline.UTC().Format(time.RFC3339Nano)
}

// inheritedHookDeadline returns the deadline gc hook run handed this process,
// and false without one. A value that does not parse is an error.
func inheritedHookDeadline() (time.Time, bool, error) {
	raw := strings.TrimSpace(os.Getenv(hookDeadlineEnv))
	if raw == "" {
		return time.Time{}, false, nil
	}
	deadline, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("parsing %s=%q: %w", hookDeadlineEnv, raw, err)
	}
	return deadline, true, nil
}

// childHookDeadline is the deadline gc hook run hands its child: ctx's, or the
// one this process inherited when that is earlier. A gc hook run nested in
// another cannot keep its child's output past the outer run's deadline, so its
// own longer timeout must not tell the child it has more time. An inherited
// value that does not parse, or that has already passed, is ignored; ctx's
// deadline still holds. A passed one is stale — a live outer run would already
// have discarded this run's output — and honoring it would skip every archive
// and release every nudge on every prompt.
func childHookDeadline(ctx context.Context) time.Time {
	deadline, ok := ctx.Deadline()
	if inherited, has, err := inheritedHookDeadline(); err == nil && has && inherited.After(time.Now()) && (!ok || inherited.Before(deadline)) {
		return inherited
	}
	return deadline
}

// hookArchiveMargin is the time left before deadline that an archive needs: a
// third of the time the process was given from start, capped at
// hookDeadlineArchiveMarginMax. A fixed margin as large as a short hook
// timeout would never let that hook archive, and the handoff it injects would
// be injected again by every hook after it.
func hookArchiveMargin(deadline, start time.Time) time.Duration {
	return min(hookDeadlineArchiveMarginMax, deadline.Sub(start)/3)
}

// hookDeadlineLeavesArchiveTime reports whether, at now, the archive margin
// remains before the deadline gc hook run handed this process, which started
// at start. Without a deadline there is no limit. A deadline that does not
// parse is reported as an error, with no time assumed left: the caller cannot
// know whether its output will be kept.
func hookDeadlineLeavesArchiveTime(now, start time.Time) (bool, error) {
	deadline, ok, err := inheritedHookDeadline()
	if err != nil {
		return false, err
	}
	if !ok {
		return true, nil
	}
	return deadline.Sub(now) >= hookArchiveMargin(deadline, start), nil
}

// hookDeadlineStillLeavesTime reports, now, whether the archive margin still
// remains before the deadline gc hook run handed this process. It gates the
// optional writes that follow a destructive step already taken — the archive's
// label, the nudge delivery stamp — so they are skipped silently, never run
// into the deadline. A deadline that does not parse leaves no time.
func hookDeadlineStillLeavesTime() bool {
	ok, err := hookDeadlineLeavesArchiveTime(time.Now(), hookProcessStart)
	return err == nil && ok
}

// hookDeadlineAllowsArchive reports whether an injecting hook has time to
// archive the injected auto-handoff mail it is about to archive, described by
// what. When it does not, it says so on stderr under cmdName, so a skipped
// archive is visible rather than silent. Callers ask only when there is
// auto-handoff mail to archive, so the warning names a real skip.
func hookDeadlineAllowsArchive(cmdName, what string, stderr io.Writer) bool {
	ok, err := hookDeadlineLeavesArchiveTime(time.Now(), hookProcessStart)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v; leaving %s unarchived\n", cmdName, err, what) //nolint:errcheck // best-effort stderr
		return false
	}
	if !ok {
		fmt.Fprintf(stderr, "%s: hook deadline too close to archive %s; it stays unread and the next hook delivers it again\n", cmdName, what) //nolint:errcheck // best-effort stderr
		return false
	}
	return true
}

// hookDeadlineAllowsNudgeAck reports whether an injecting nudge drain has time
// to ack the nudges it just wrote, described by what, and exit before the
// deadline gc hook run handed it. When it does not, it says so on stderr: the
// caller releases the claims instead, so the nudges are delivered again.
func hookDeadlineAllowsNudgeAck(what string, stderr io.Writer) bool {
	ok, err := hookDeadlineLeavesArchiveTime(time.Now(), hookProcessStart)
	if err != nil {
		fmt.Fprintf(stderr, "gc nudge drain: %v; releasing %s for redelivery\n", err, what) //nolint:errcheck // best-effort stderr
		return false
	}
	if !ok {
		fmt.Fprintf(stderr, "gc nudge drain: hook deadline too close to ack %s; releasing them for redelivery by the next drain\n", what) //nolint:errcheck // best-effort stderr
		return false
	}
	return true
}
