package orders

import (
	"context"
	"encoding/json"
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
	case "webhook":
		// Webhook-triggered orders are dispatched only by the supervisor webhook
		// receiver; like manual, they are never tick-fired.
		return TriggerResult{Due: false, Reason: "webhook trigger — dispatched by the webhook receiver"}
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

// resolveOrderLocation returns the single explicit location in which an
// order's cron fields are evaluated: the order's tz (authored in the order
// file, or the city-wide [workspace] timezone stamped onto the order at scan
// time), falling back to `now`'s location when no tz is configured. For the
// live dispatcher `now` is time.Now(), so the fallback is the process-local
// zone — the pre-fix live-match semantics — while callers that fabricate
// times in an explicit location (tests, replay) stay deterministic
// regardless of the host zone. A bad tz is a hard error — order validation
// rejects it at load; this guard keeps an unvalidated Order from silently
// evaluating in the wrong zone.
func resolveOrderLocation(a Order, now time.Time) (*time.Location, error) {
	if a.TZ == "" {
		return now.Location(), nil
	}
	loc, err := time.LoadLocation(a.TZ)
	if err != nil {
		return nil, fmt.Errorf("order %q: invalid tz %q: %w", a.ScopedName(), a.TZ, err)
	}
	return loc, nil
}

// wallMinuteLayout renders a wall-clock reading to minute granularity.
// Two instants with the same rendering occupy the same wall-clock slot —
// including the DST fall-back hour, where two distinct instants share one
// wall-clock reading and must count as a single cron slot.
const wallMinuteLayout = "2006-01-02 15:04"

// checkCron uses minute-granularity matching against the schedule, WITH
// catch-up. A scheduled occurrence fires if either (a) the current minute
// matches, or (b) a scheduled minute elapsed since the last run without the
// controller evaluating during that exact minute. Catch-up mirrors cooldown's
// elapsed-based behavior: without it, a cron order silently drops a slot
// whenever no evaluation lands in its matching minute, which made a
// "0 */4 * * *" order miss every boundary (gastown td-4kziysy) because the
// controller's eval cadence rarely coincides with a once-per-4h minute.
// Schedule format: "minute hour day-of-month month day-of-week" (5 fields).
//
// All cron-field evaluation happens in ONE explicit location (see
// resolveOrderLocation). Callers and the last-run store may hand us times in
// different locations — the doltlite store always returns UTC-located times
// (parseTimeString) while `now` carries the process zone — so both are
// normalized here before any field is read. Without this, the catch-up scan
// evaluated cron fields against the store's UTC wall clock and fired
// zone-anchored orders at the UTC reading, then again at the real local slot.
//
// DST policy (in the resolved location):
//   - Fall-back: the repeated hour yields two instants with the same
//     wall-clock reading; an order fires at most once per wall-clock slot
//     (dedupe by wall-clock date+HH:MM against lastRun).
//   - Spring-forward: schedule minutes inside the nonexistent hour cannot
//     match a real instant; the catch-up scan detects the gap and fires the
//     order once at the first real minute after the jump.
func checkCron(a Order, now time.Time, lastRunFn LastRunFunc) TriggerResult {
	fields := strings.Fields(a.Schedule)
	if len(fields) != 5 {
		return TriggerResult{Due: false, Reason: fmt.Sprintf("bad cron schedule: want 5 fields, got %d", len(fields))}
	}

	minute, hour, dom, month, dow := fields[0], fields[1], fields[2], fields[3], fields[4]

	loc, err := resolveOrderLocation(a, now)
	if err != nil {
		return TriggerResult{Due: false, Reason: fmt.Sprintf("bad tz: %v", err)}
	}
	now = now.In(loc)

	matchesAt := func(t time.Time) bool {
		return cronFieldMatches(minute, t.Minute()) &&
			cronFieldMatches(hour, t.Hour()) &&
			cronFieldMatches(dom, t.Day()) &&
			cronFieldMatches(month, int(t.Month())) &&
			cronFieldMatches(dow, int(t.Weekday()))
	}
	sameWallMinute := func(x, y time.Time) bool {
		return x.Format(wallMinuteLayout) == y.Format(wallMinuteLayout)
	}

	last, err := lastRunFn(a.ScopedName())
	if err != nil {
		return TriggerResult{Due: false, Reason: fmt.Sprintf("error querying last run: %v", err)}
	}
	last = last.In(loc) // same instant, evaluator's wall clock (IsZero is instant-based, unaffected)

	// (a) Current minute matches — fire unless already run this wall-clock
	// slot (wall-minute equality also covers the DST fall-back repeat, where
	// two instants an hour apart share one wall-clock reading).
	if matchesAt(now) {
		if !last.IsZero() && sameWallMinute(last, now) {
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
		prev := start.Add(-time.Minute)
		for t := start; !t.After(now); t = t.Add(time.Minute) {
			// Spring-forward: one absolute minute stepped over a wall-clock
			// gap (e.g. 01:59 → 03:00). Schedule minutes inside the gap can
			// never match a real instant, so evaluate the skipped wall-clock
			// readings and fire at this first real minute after the jump.
			_, prevOff := prev.Zone()
			_, tOff := t.Zone()
			if tOff > prevOff && matchesInWallGap(matchesAt, prev, t) {
				return TriggerResult{Due: true, Reason: "cron: caught up occurrence skipped by DST spring-forward", LastRun: last}
			}
			if matchesAt(t) && !sameWallMinute(last, t) {
				return TriggerResult{Due: true, Reason: "cron: caught up missed occurrence", LastRun: last}
			}
			prev = t
		}
	}

	return TriggerResult{Due: false, Reason: "cron: schedule not matched", LastRun: last}
}

// matchesInWallGap reports whether any wall-clock minute strictly between
// prev's and t's wall-clock readings matches the schedule. Such readings do
// not exist as instants in the location (a DST spring-forward skipped them),
// so they are enumerated as naive calendar readings in a fixed-offset
// container; cron fields are pure wall-clock components, so matching them
// against naive readings is exact.
func matchesInWallGap(matchesAt func(time.Time) bool, prev, t time.Time) bool {
	naive := func(x time.Time) time.Time {
		return time.Date(x.Year(), x.Month(), x.Day(), x.Hour(), x.Minute(), 0, 0, time.UTC)
	}
	for w, end := naive(prev).Add(time.Minute), naive(t); w.Before(end); w = w.Add(time.Minute) {
		if matchesAt(w) {
			return true
		}
	}
	return false
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
// Events emitted by order-tracking beads (controller bookkeeping) are excluded
// to prevent event orders from self-firing on their own tracking-bead lifecycle.
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
	var count int
	for _, e := range matched {
		// Exclude the dispatcher's own order-tracking bookkeeping beads so an event
		// order never self-fires on lifecycle events emitted by those beads (#3720).
		if !payloadHasLabel(e.Payload, labelOrderTracking) {
			count++
		}
	}
	if count == 0 {
		return TriggerResult{Due: false, Reason: "event: no matching events"}
	}
	return TriggerResult{Due: true, Reason: fmt.Sprintf("event: %d %s event(s)", count, a.On)}
}

// payloadHasLabel reports whether a JSON bead payload contains the given label.
func payloadHasLabel(payload json.RawMessage, label string) bool {
	if len(payload) == 0 {
		return false
	}
	var p struct {
		Labels []string `json:"labels"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return false
	}
	for _, l := range p.Labels {
		if l == label {
			return true
		}
	}
	return false
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
