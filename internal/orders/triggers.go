package orders

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/execenv"
)

// TriggerResult holds the outcome of a trigger check.
type TriggerResult struct {
	// Due is true if the trigger condition is satisfied and the order should run.
	Due bool
	// Reason explains why the trigger is or isn't due.
	Reason string
	// LastRun is the last execution time (zero if never run).
	LastRun time.Time
}

// LastRunFunc returns the last run time for a named order.
// Returns zero time and nil error if never run.
type LastRunFunc func(name string) (time.Time, error)

// CursorFunc returns the event cursor (highest seq) for a named order.
// Returns 0 if no cursor exists.
type CursorFunc func(orderName string) uint64

// TriggerOptions carries execution context for triggers that run subprocesses.
type TriggerOptions struct {
	ConditionDir     string
	ConditionEnv     []string
	ConditionTimeout time.Duration
}

var (
	// conditionCheckPostCancelWaitDelay is os/exec's pipe-close wait after
	// Cancel returns; the TERM and KILL waits each use conditionCheckSignalGrace.
	conditionCheckPostCancelWaitDelay = 2 * time.Second
	conditionCheckSignalGrace         = 2 * time.Second
)

// CheckTrigger evaluates an order's trigger condition and returns whether it's due.
// ep is an events Provider used by event triggers to query events; may be nil for
// non-event triggers.
// cursorFn returns the last-processed event seq for event triggers; may be nil for
// non-event triggers.
func CheckTrigger(a Order, now time.Time, lastRunFn LastRunFunc, ep events.Provider, cursorFn CursorFunc) TriggerResult {
	return CheckTriggerWithOptions(a, now, lastRunFn, ep, cursorFn, TriggerOptions{})
}

// CheckTriggerWithOptions evaluates an order trigger using explicit execution
// context for condition checks.
func CheckTriggerWithOptions(a Order, now time.Time, lastRunFn LastRunFunc, ep events.Provider, cursorFn CursorFunc, opts TriggerOptions) TriggerResult {
	switch a.Trigger {
	case "cooldown":
		return checkCooldown(a, now, lastRunFn)
	case "cron":
		return checkCron(a, now, lastRunFn)
	case "condition":
		return checkCondition(a, opts)
	case "event":
		return checkEvent(a, ep, cursorFn)
	case "manual":
		return TriggerResult{Due: false, Reason: "manual trigger — use gc order run"}
	default:
		return TriggerResult{Due: false, Reason: fmt.Sprintf("unknown trigger %q", a.Trigger)}
	}
}

// checkCooldown checks if enough time has elapsed since the last run.
func checkCooldown(a Order, now time.Time, lastRunFn LastRunFunc) TriggerResult {
	interval, err := time.ParseDuration(a.Interval)
	if err != nil {
		return TriggerResult{Due: false, Reason: fmt.Sprintf("bad interval: %v", err)}
	}

	last, err := lastRunFn(a.ScopedName())
	if err != nil {
		return TriggerResult{Due: false, Reason: fmt.Sprintf("error querying last run: %v", err)}
	}

	if last.IsZero() {
		return TriggerResult{Due: true, Reason: "never run", LastRun: last}
	}

	elapsed := now.Sub(last)
	if elapsed >= interval {
		return TriggerResult{
			Due:     true,
			Reason:  fmt.Sprintf("elapsed %s >= interval %s", elapsed.Round(time.Second), interval),
			LastRun: last,
		}
	}

	remaining := interval - elapsed
	return TriggerResult{
		Due:     false,
		Reason:  fmt.Sprintf("cooldown: %s remaining", remaining.Round(time.Second)),
		LastRun: last,
	}
}

// checkCron uses minute-granularity matching against the schedule, WITH
// catch-up. A scheduled occurrence fires if either (a) the current minute
// matches, or (b) a scheduled minute elapsed since the last run without the
// controller evaluating during that exact minute. Catch-up mirrors cooldown's
// elapsed-based behavior: without it, a cron order silently drops a slot
// whenever no evaluation lands in its matching minute, which made a
// "0 */4 * * *" order miss every boundary (gastown td-4kziysy) because the
// controller's eval cadence rarely coincides with a once-per-4h minute.
// Schedule format: "minute hour day-of-month month day-of-week" (5 fields).
func checkCron(a Order, now time.Time, lastRunFn LastRunFunc) TriggerResult {
	fields := strings.Fields(a.Schedule)
	if len(fields) != 5 {
		return TriggerResult{Due: false, Reason: fmt.Sprintf("bad cron schedule: want 5 fields, got %d", len(fields))}
	}

	minute, hour, dom, month, dow := fields[0], fields[1], fields[2], fields[3], fields[4]

	matchesAt := func(t time.Time) bool {
		return cronFieldMatches(minute, t.Minute()) &&
			cronFieldMatches(hour, t.Hour()) &&
			cronFieldMatches(dom, t.Day()) &&
			cronFieldMatches(month, int(t.Month())) &&
			cronFieldMatches(dow, int(t.Weekday()))
	}

	last, err := lastRunFn(a.ScopedName())
	if err != nil {
		return TriggerResult{Due: false, Reason: fmt.Sprintf("error querying last run: %v", err)}
	}

	// (a) Current minute matches — fire unless already run this minute.
	if matchesAt(now) {
		if !last.IsZero() && last.Truncate(time.Minute).Equal(now.Truncate(time.Minute)) {
			return TriggerResult{Due: false, Reason: "cron: already run this minute", LastRun: last}
		}
		return TriggerResult{Due: true, Reason: "cron: schedule matched", LastRun: last}
	}

	// (b) Catch-up: the current minute does not match, but a scheduled minute
	// may have elapsed since lastRun without an evaluation landing on it. Scan
	// minute-by-minute from just after lastRun up to now; any match is a missed
	// occurrence that is now due. Bounded lookback so a very old lastRun cannot
	// spin (it is overdue regardless). Skipped when lastRun is zero (never run):
	// such an order fires only on an exact match, never back-filling history.
	if !last.IsZero() {
		const maxCatchupLookback = 366 * 24 * time.Hour
		start := last.Truncate(time.Minute).Add(time.Minute)
		if floor := now.Add(-maxCatchupLookback).Truncate(time.Minute); start.Before(floor) {
			start = floor
		}
		for t := start; !t.After(now); t = t.Add(time.Minute) {
			if matchesAt(t) {
				return TriggerResult{Due: true, Reason: "cron: caught up missed occurrence", LastRun: last}
			}
		}
	}

	return TriggerResult{Due: false, Reason: "cron: schedule not matched", LastRun: last}
}

// cronFieldMatches checks if a single cron field matches a value.
// Supports: "*" (any), exact integer, or comma-separated values.
func cronFieldMatches(field string, value int) bool {
	if field == "*" {
		return true
	}
	for _, part := range strings.Split(field, ",") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "*/") {
			step, err := strconv.Atoi(strings.TrimPrefix(part, "*/"))
			if err == nil && step > 0 && value%step == 0 {
				return true
			}
			continue
		}
		n, err := strconv.Atoi(part)
		if err == nil && n == value {
			return true
		}
	}
	return false
}

// checkCondition runs the check command and returns due if exit code is 0.
// Uses a timeout to prevent hanging check scripts from blocking trigger evaluation.
func checkCondition(a Order, opts TriggerOptions) TriggerResult {
	const triggerCheckTimeout = 10 * time.Second
	timeout := opts.ConditionTimeout
	if timeout <= 0 {
		timeout = triggerCheckTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", a.Check)
	cleanupCommand := prepareConditionCommand(cmd, conditionCheckSignalGrace)
	cmd.WaitDelay = conditionCheckPostCancelWaitDelay
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if opts.ConditionDir != "" {
		cmd.Dir = opts.ConditionDir
	}
	cmd.Env = mergeConditionEnv(os.Environ(), opts.ConditionEnv)
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			reason := fmt.Sprintf("check command timed out after %s", timeout)
			if cleanupErr := cleanupCommand(); cleanupErr != nil {
				reason = fmt.Sprintf("%s; cleanup failed: %v", reason, cleanupErr)
			}
			return TriggerResult{Due: false, Reason: reason}
		}
		if errors.Is(err, exec.ErrWaitDelay) {
			reason := "check command cleanup exceeded post-cancel wait delay"
			if cleanupErr := cleanupCommand(); cleanupErr != nil {
				reason = fmt.Sprintf("%s: %v", reason, cleanupErr)
			}
			return TriggerResult{Due: false, Reason: reason}
		}
		return TriggerResult{Due: false, Reason: fmt.Sprintf("check command failed: %v", err)}
	}
	return TriggerResult{Due: true, Reason: "condition: check passed (exit 0)"}
}

func mergeConditionEnv(environ, extra []string) []string {
	return execenv.MergeEntries(environ, extra)
}

// checkEvent checks if matching events exist after the last cursor position.
func checkEvent(a Order, ep events.Provider, cursorFn CursorFunc) TriggerResult {
	if ep == nil {
		return TriggerResult{Due: false, Reason: "event: no events provider"}
	}
	var cursor uint64
	if cursorFn != nil {
		cursor = cursorFn(a.ScopedName())
	}

	matched, err := ep.List(events.Filter{
		Type:     a.On,
		AfterSeq: cursor,
	})
	if err != nil {
		return TriggerResult{Due: false, Reason: fmt.Sprintf("event: read error: %v", err)}
	}
	if len(matched) == 0 {
		return TriggerResult{Due: false, Reason: "event: no matching events"}
	}
	return TriggerResult{Due: true, Reason: fmt.Sprintf("event: %d %s event(s)", len(matched), a.On)}
}

// MaxSeqFromLabels extracts the highest seq:<N> value from bead labels.
// Used by CLI callers to compute the event cursor from BdStore results.
func MaxSeqFromLabels(labelSets [][]string) uint64 {
	var maxSeq uint64
	for _, labels := range labelSets {
		for _, l := range labels {
			if strings.HasPrefix(l, "seq:") {
				if n, err := strconv.ParseUint(l[4:], 10, 64); err == nil && n > maxSeq {
					maxSeq = n
				}
			}
		}
	}
	return maxSeq
}
