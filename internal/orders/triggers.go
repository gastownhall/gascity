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
	// ConditionCtx is the parent context for the condition-check subprocess.
	// When non-nil, canceling it — a controller shutdown, a config reload, or a
	// canceled dispatch tick — interrupts a running check promptly instead of
	// letting a raised check_timeout keep the process alive for the full
	// deadline. Bare callers (the API GET /v0/orders/check evaluator and the
	// storeless CLI check) may leave it nil; checkCondition then falls back to
	// context.Background(), preserving the timeout-only behavior for those
	// one-shot evaluators.
	ConditionCtx     context.Context
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

// ConditionCheckTimedOutMarker is the substring embedded in a condition
// trigger's TriggerResult.Reason when the check command is killed by its
// check_timeout deadline. The dispatcher matches on it to emit the
// operator-facing starvation diagnostic, so both the producer here and the
// consumer in the dispatcher reference this one constant instead of coupling
// on a separately-typed literal across packages.
const ConditionCheckTimedOutMarker = "timed out"

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
			cronDayOfWeekMatches(dow, int(t.Weekday()))
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
	// spin (it is overdue regardless).
	//
	// A never-run order (lastRun zero) gets the same scan bounded to a much
	// shorter lookback instead of being skipped entirely: without it, a
	// narrow-window order (e.g. one specific minute/day) that never happens to
	// be evaluated on its exact scheduled minute could never fire at all — no
	// lastRun exists to catch up from, and no restart recovers it (#3947, a
	// residual cold-start gap in the warm-order catch-up above). The short
	// floor keeps a freshly-enabled order from firing for an occurrence that
	// elapsed long before it existed, unlike the year-long lookback a warm
	// order's lastRun-anchored catch-up gets.
	const maxCatchupLookback = 366 * 24 * time.Hour
	const neverRunCatchupLookback = 25 * time.Hour
	lookback := maxCatchupLookback
	start := last.Truncate(time.Minute).Add(time.Minute)
	if last.IsZero() {
		lookback = neverRunCatchupLookback
		start = now.Add(-neverRunCatchupLookback).Truncate(time.Minute)
	}
	if floor := now.Add(-lookback).Truncate(time.Minute); start.Before(floor) {
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

// cronField describes one positional field of a schedule: its human name and
// the closed value range checkCron will ever test it against. Values outside
// that range can never match — an "hour" of 25 is as dead as a syntax error —
// so validation rejects both.
type cronField struct {
	name     string
	low      int
	high     int
	extraMsg string
}

// cronFields are the five schedule fields, in order. Day-of-week runs to 7
// because 7 is the common cron spelling of Sunday: time.Weekday never yields
// it, so cronDayOfWeekMatches probes such a schedule at both readings rather
// than letting it validate cleanly and never fire.
var cronFields = [5]cronField{
	{name: "minute", low: 0, high: 59},
	{name: "hour", low: 0, high: 23},
	{name: "day-of-month", low: 1, high: 31},
	{name: "month", low: 1, high: 12},
	{name: "day-of-week", low: 0, high: 7, extraMsg: " (0 or 7 = Sunday)"},
}

// ValidateCronSchedule reports whether a schedule is expressible in the
// grammar cronFieldMatches implements. It exists because every rejection here
// was, before it, an order that loaded cleanly and then never ran: the
// evaluator has no way to signal "I did not understand this field", it just
// fails to match, forever. Order validation calls this at load so the failure
// lands where someone is looking.
func ValidateCronSchedule(schedule string) error {
	fields := strings.Fields(schedule)
	if len(fields) != 5 {
		return fmt.Errorf("cron schedule %q: want 5 fields (minute hour day-of-month month day-of-week), got %d", schedule, len(fields))
	}
	for i, field := range fields {
		if err := validateCronField(cronFields[i], field); err != nil {
			return err
		}
	}
	return nil
}

func validateCronField(f cronField, field string) error {
	if field == "" {
		return fmt.Errorf("cron %s field is empty", f.name)
	}
	for _, term := range strings.Split(field, ",") {
		if err := validateCronTerm(f, strings.TrimSpace(term)); err != nil {
			return err
		}
	}
	return nil
}

// validateCronTerm mirrors cronTermMatches term for term. Any change to one
// belongs in the other: a term this accepts but that cannot match is exactly
// the silent-never-fires bug this validation exists to prevent.
func validateCronTerm(f cronField, term string) error {
	bad := func(reason string) error {
		return fmt.Errorf("cron %s field: %q %s%s", f.name, term, reason, f.extraMsg)
	}

	spec, stepText, hasStep := strings.Cut(term, "/")
	step := 1
	if hasStep {
		n, err := strconv.Atoi(strings.TrimSpace(stepText))
		if err != nil {
			return bad("has a non-numeric step")
		}
		if n <= 0 {
			return bad("has a step that is not positive")
		}
		step = n
	}
	spec = strings.TrimSpace(spec)
	if spec == "*" {
		// "*/N" anchors its stride at 0, so in a field whose low bound is 1
		// the smallest value it can produce is N itself. Once N passes the
		// field's high bound the matching set is empty and the schedule is
		// dead — the same silent never-fires this validation exists to catch.
		// "A-B/N" needs no equivalent check: it anchors at A, which always
		// matches.
		if hasStep && f.low > 0 && step > f.high {
			return bad(fmt.Sprintf(`has no value in %d-%d (a "*/N" step counts from 0)`, f.low, f.high))
		}
		return nil
	}

	inBounds := func(v int) error {
		if v < f.low || v > f.high {
			return bad(fmt.Sprintf("is outside the valid range %d-%d", f.low, f.high))
		}
		return nil
	}

	lowText, highText, isRange := strings.Cut(spec, "-")
	low, err := strconv.Atoi(strings.TrimSpace(lowText))
	if err != nil {
		return bad(`is not "*", an integer, a range "A-B", or a step of either`)
	}
	if err := inBounds(low); err != nil {
		return err
	}
	if !isRange {
		if hasStep {
			return bad(`uses a step without a range; write "A-B/N" or "*/N"`)
		}
		return nil
	}
	high, err := strconv.Atoi(strings.TrimSpace(highText))
	if err != nil {
		return bad("has a non-numeric range end")
	}
	if err := inBounds(high); err != nil {
		return err
	}
	if high < low {
		return bad("has a range that ends before it starts")
	}
	return nil
}

// cronFieldMatches checks if a single cron field matches a value.
// Supports: "*" (any), exact integer, comma-separated lists, "*/N" strides,
// "A-B" ranges, and "A-B/N" stepped ranges.
//
// An unrecognized term matches nothing, and nothing distinguishes that from
// an ordinary non-match: no error, no log line, just an order that stays
// registered and never fires. ValidateCronSchedule rejects any term this
// function cannot express, so a gap in the grammar surfaces as a load error
// rather than a silently dead schedule.
func cronFieldMatches(field string, value int) bool {
	for _, part := range strings.Split(field, ",") {
		if cronTermMatches(strings.TrimSpace(part), value) {
			return true
		}
	}
	return false
}

// cronDayOfWeekMatches accepts 7 as an alias for Sunday. time.Weekday never
// yields 7, so a schedule written with the common cron spelling is probed at
// both readings rather than silently never firing.
func cronDayOfWeekMatches(field string, weekday int) bool {
	if cronFieldMatches(field, weekday) {
		return true
	}
	return weekday == 0 && cronFieldMatches(field, 7)
}

// cronTermMatches evaluates one comma-separated term of a cron field.
// An unparseable term matches nothing; keep it in lockstep with
// validateCronTerm, which decides what callers are allowed to write.
func cronTermMatches(term string, value int) bool {
	spec, stepText, hasStep := strings.Cut(term, "/")
	step := 1
	if hasStep {
		n, err := strconv.Atoi(strings.TrimSpace(stepText))
		if err != nil || n <= 0 {
			return false
		}
		step = n
	}
	spec = strings.TrimSpace(spec)

	if spec == "*" {
		// Historic gc semantics for "*/N": the stride is anchored at 0 rather
		// than at the field's minimum. Preserved deliberately — changing it
		// would move the slots of every "*/N" schedule already in service.
		return value%step == 0
	}

	lowText, highText, isRange := strings.Cut(spec, "-")
	low, err := strconv.Atoi(strings.TrimSpace(lowText))
	if err != nil {
		return false
	}
	if !isRange {
		// A bare "N/step" has no upper bound to stride against; reject it
		// rather than guess. validateCronTerm refuses it too.
		return !hasStep && low == value
	}
	high, err := strconv.Atoi(strings.TrimSpace(highText))
	if err != nil || high < low {
		return false
	}
	if value < low || value > high {
		return false
	}
	return (value-low)%step == 0
}

// checkCondition runs the check command and returns due if exit code is 0.
// Uses a timeout to prevent hanging check scripts from blocking trigger evaluation.
func checkCondition(a Order, opts TriggerOptions) TriggerResult {
	timeout := opts.ConditionTimeout
	if timeout <= 0 {
		// Derive the deadline from the order itself so every CheckTrigger
		// caller honors check_timeout, not only the ones that populate
		// TriggerOptions.ConditionTimeout (controller dispatch, store-aware
		// CLI check). Bare callers — the API /v0/orders/check evaluator and
		// the storeless CLI check — pass empty opts; CheckTimeoutOrDefault
		// returns defaultConditionCheckTimeout for an unset/invalid value, so
		// this preserves the prior 10s behavior when check_timeout is absent.
		timeout = a.CheckTimeoutOrDefault()
	}
	// Derive the check deadline from the caller's context when one is supplied so
	// a canceled tick/shutdown/reload stops a running check before check_timeout
	// elapses; nil opts (bare CLI/API evaluators) fall back to the background
	// context, keeping the timeout as the sole bound.
	parent := opts.ConditionCtx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
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
			reason := fmt.Sprintf("check command %s after %s", ConditionCheckTimedOutMarker, timeout)
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
