package orders

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/execenv"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TriggerResult holds the outcome of a trigger check.
type TriggerResult struct {
	// Due is true if the trigger condition is satisfied and the order should run.
	Due bool
	// Reason explains why the trigger is or isn't due.
	Reason string
	// TimedOut is true only when a condition check was killed by its check_timeout deadline.
	TimedOut bool
	// ReadFailed is true only when an event check could not read the event
	// log (its head or its events): Due is then false because the read
	// failed, not because no event matched.
	ReadFailed bool
	// Err is the contextual read error behind ReadFailed, nil otherwise.
	Err error
	// CursorReset is set when an event check discarded a run stamp above the
	// event log's head (the log restarted below it): the check read from the
	// highest stamp the log can contain instead. Nil when nothing was
	// discarded.
	CursorReset *CursorReset
	// LastRun is the last execution time (zero if never run).
	LastRun time.Time
}

// LastRunFunc returns the last run time for a named order.
// Returns zero time and nil error if never run.
type LastRunFunc func(name string) (time.Time, error)

// CursorFunc returns the event seq stamps (seq:<N> labels) of a named order's
// run records, in any order; nil when it has none. The event check reduces
// them to its cursor against the event log's head (see CursorReset).
type CursorFunc func(orderName string) []uint64

// CursorReset describes an event check that found run stamps above the event
// log's head, as when the log was lost or replaced while the bead store kept
// its run records: Stamp is the highest discarded stamp, Head the log's head,
// and Cursor the seq the check read past (the highest stamp at or below Head,
// or 0).
type CursorReset struct {
	Stamp  uint64
	Head   uint64
	Cursor uint64
}

// String names the discarded stamp, the head and the cursor read from.
func (r CursorReset) String() string {
	return fmt.Sprintf("cursor reset: run stamp seq %d is above the event log head %d; reading after seq %d", r.Stamp, r.Head, r.Cursor)
}

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
// check_timeout deadline. It is only the human-readable text in Reason;
// callers that need to detect a timeout must branch on TriggerResult.TimedOut,
// since stderr excerpts appended to Reason can contain the same words.
const ConditionCheckTimedOutMarker = "timed out"

const (
	conditionCheckStderrTailBytes    = 4096
	conditionCheckStderrExcerptRunes = 300
)

type boundedTailWriter struct {
	buf     []byte
	limit   int
	dropped bool
	written int
}

func newBoundedTailWriter(limit int) *boundedTailWriter {
	return &boundedTailWriter{buf: make([]byte, 0, limit), limit: limit}
}

func (w *boundedTailWriter) Write(p []byte) (int, error) {
	n := len(p)
	w.written += n
	if len(w.buf)+n > w.limit {
		w.dropped = true
	}
	if n >= w.limit {
		w.buf = w.buf[:w.limit]
		copy(w.buf, p[n-w.limit:])
		return n, nil
	}
	if overflow := len(w.buf) + n - w.limit; overflow > 0 {
		copy(w.buf, w.buf[overflow:])
		w.buf = w.buf[:len(w.buf)-overflow]
	}
	w.buf = append(w.buf, p...)
	return n, nil
}

func (w *boundedTailWriter) String() string {
	return string(w.buf)
}

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
	interval = clock.Backstop(interval)

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
	if err := ValidateCronSchedule(a.Schedule); err != nil {
		return TriggerResult{Due: false, Reason: fmt.Sprintf("bad cron schedule: %v", err)}
	}
	fields := strings.Fields(a.Schedule)

	loc, err := resolveOrderLocation(a, now)
	if err != nil {
		return TriggerResult{Due: false, Reason: fmt.Sprintf("bad tz: %v", err)}
	}
	now = now.In(loc)

	// The schedule was validated above, so no parse error can reach here; a
	// non-match is the safe reading if one ever did.
	matchesAt := func(t time.Time) bool {
		matched, err := CronScheduleMatchesAt(fields, t)
		return err == nil && matched
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
	if reason := runtime.ShellCommandUnavailable(a.Check); reason != "" {
		return TriggerResult{Due: false, Reason: "check command refused: " + reason}
	}
	cmd, err := runtime.ShellCommandContext(ctx, a.Check)
	if err != nil {
		return TriggerResult{Due: false, Reason: fmt.Sprintf("check command failed: %v", err)}
	}
	cleanupCommand := prepareConditionCommand(cmd, conditionCheckSignalGrace)
	cmd.WaitDelay = conditionCheckPostCancelWaitDelay
	cmd.Stdout = io.Discard
	cmd.Env = mergeConditionEnv(os.Environ(), opts.ConditionEnv)
	secrets := conditionCheckSensitiveValues(cmd.Env)
	captureBytes := conditionCheckStderrTailBytes
	if len(secrets) > 0 {
		captureBytes += len(secrets[0])
	}
	stderrTail := newBoundedTailWriter(captureBytes)
	cmd.Stderr = stderrTail
	if opts.ConditionDir != "" {
		cmd.Dir = opts.ConditionDir
	}
	err = startConditionCommand(cmd)
	if err == nil {
		defer releaseConditionCommand(cmd)
		err = cmd.Wait()
	}
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			reason := fmt.Sprintf("check command %s after %s", ConditionCheckTimedOutMarker, timeout)
			if cleanupErr := cleanupCommand(); cleanupErr != nil {
				reason = fmt.Sprintf("%s; cleanup failed: %v", reason, cleanupErr)
			}
			return TriggerResult{Due: false, Reason: reason, TimedOut: true}
		}
		if errors.Is(err, exec.ErrWaitDelay) {
			reason := "check command cleanup exceeded post-cancel wait delay"
			if cleanupErr := cleanupCommand(); cleanupErr != nil {
				reason = fmt.Sprintf("%s: %v", reason, cleanupErr)
			}
			return TriggerResult{Due: false, Reason: reason}
		}
		excerpt := conditionCheckStderrExcerpt(
			stderrTail.String(),
			stderrTail.dropped,
			stderrTail.written > conditionCheckStderrTailBytes,
			cmd.Env,
			secrets,
		)
		var exitErr *exec.ExitError
		if excerpt == "" && ctx.Err() == nil && errors.As(err, &exitErr) && exitErr.Exited() {
			return TriggerResult{Due: false, Reason: fmt.Sprintf("condition: not met (exit %d)", exitErr.ExitCode())}
		}
		reason := fmt.Sprintf("check command failed: %v", err)
		if excerpt != "" {
			reason += ": stderr: " + excerpt
		}
		return TriggerResult{Due: false, Reason: reason}
	}
	return TriggerResult{Due: true, Reason: "condition: check passed (exit 0)"}
}

func conditionCheckStderrExcerpt(stderr string, captureDropped, overLimit bool, env, secrets []string) string {
	excerpt := execenv.RedactText(stderr, env)
	for _, secret := range secrets {
		if len(secret) < 4 {
			excerpt = strings.ReplaceAll(excerpt, secret, execenv.Redacted)
		}
	}
	partialLine := captureDropped
	if len(excerpt) > conditionCheckStderrTailBytes {
		excerpt = excerpt[len(excerpt)-conditionCheckStderrTailBytes:]
		partialLine = true
	}
	if partialLine {
		if newline := strings.IndexByte(excerpt, '\n'); newline >= 0 {
			excerpt = excerpt[newline+1:]
		} else {
			excerpt = ""
		}
	}
	excerpt = strings.Map(func(r rune) rune {
		if !unicode.IsPrint(r) {
			return ' '
		}
		return r
	}, excerpt)
	excerpt = strings.TrimSpace(excerpt)
	if excerpt == "" && !overLimit {
		return ""
	}
	runes := []rune(excerpt)
	if !overLimit && len(runes) <= conditionCheckStderrExcerptRunes {
		return excerpt
	}
	if keep := conditionCheckStderrExcerptRunes - 1; len(runes) > keep {
		runes = runes[len(runes)-keep:]
	}
	return "…" + string(runes)
}

func conditionCheckSensitiveValues(env []string) []string {
	seen := map[string]struct{}{}
	values := make([]string, 0)
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		value = strings.TrimSpace(value)
		if !ok || value == "" || !execenv.IsSensitiveKey(key) {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		values = append(values, value)
	}
	sort.Slice(values, func(i, j int) bool {
		return len(values[i]) > len(values[j])
	})
	return values
}

func mergeConditionEnv(environ, extra []string) []string {
	return execenv.MergeEntries(environ, extra)
}

// checkEvent checks if matching events exist after the last cursor position.
// It reads the order's run stamps, then the event log's head, then the events,
// in that order: a stamp written after the head was read could otherwise be
// mistaken for one above it. Stamps above the head are discarded
// (boundCursor), so a log that restarted below them is read from the highest
// stamp it can contain. A head or list read failure returns not due with
// ReadFailed and Err set.
// Events emitted by order-tracking beads (controller bookkeeping) are excluded
// to prevent event orders from self-firing on their own tracking-bead lifecycle.
func checkEvent(a Order, ep events.Provider, cursorFn CursorFunc) TriggerResult {
	if ep == nil {
		return TriggerResult{Due: false, Reason: "event: no events provider"}
	}
	var stamps []uint64
	if cursorFn != nil {
		stamps = cursorFn(a.ScopedName())
	}
	head, err := ep.LatestSeq()
	if err != nil {
		err = fmt.Errorf("reading event head for order %s: %w", a.ScopedName(), err)
		return TriggerResult{Due: false, ReadFailed: true, Err: err, Reason: "event: " + err.Error()}
	}
	cursor, discarded := boundCursor(stamps, head)
	var reset *CursorReset
	if discarded > 0 {
		reset = &CursorReset{Stamp: discarded, Head: head, Cursor: cursor}
	}

	matched, err := listEventsWithInFlight(ep, events.Filter{
		Type:     a.On,
		AfterSeq: cursor,
	})
	if err != nil {
		return TriggerResult{
			Due:         false,
			ReadFailed:  true,
			Err:         fmt.Errorf("listing %s events for order %s after seq %d: %w", a.On, a.ScopedName(), cursor, err),
			CursorReset: reset,
			Reason:      withCursorReset(fmt.Sprintf("event: read error: %v", err), reset),
		}
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
		return TriggerResult{Due: false, CursorReset: reset, Reason: withCursorReset("event: no matching events", reset)}
	}
	return TriggerResult{Due: true, CursorReset: reset, Reason: withCursorReset(fmt.Sprintf("event: %d %s event(s)", count, a.On), reset)}
}

// withCursorReset appends reset's facts to reason; reason is unchanged when
// no stamp was discarded.
func withCursorReset(reason string, reset *CursorReset) string {
	if reset == nil {
		return reason
	}
	return reason + " (" + reset.String() + ")"
}

// boundCursor reduces an order's run stamps to the event cursor a log whose
// head is head can honor: cursor is the highest stamp at or below head (0 when
// none is), and discarded the highest stamp above it (0 when none is). A stamp
// equal to head is kept, so the events through head stay consumed.
func boundCursor(stamps []uint64, head uint64) (cursor, discarded uint64) {
	for _, seq := range stamps {
		if seq > head {
			discarded = max(discarded, seq)
			continue
		}
		cursor = max(cursor, seq)
	}
	return cursor, discarded
}

// CheckEventStart returns the stamps a standalone event check (gc order
// check, the orders/check API) reads past for an order whose run stamps are
// runStamps: the stamps themselves once the order has run, or the event head
// for an order that has never run (no stamp above 0). checkEvent bounds the
// returned stamps by the head it reads.
//
// An event order fires on the matching events recorded after the controller
// began watching it; history recorded before then is never replayed (the
// controller seeds a never-run order at the head, see cmd/gc
// eventCheckStart). A standalone check holds no record of when watching
// began, so it reads nothing below the head and reports a never-run order not
// due: a matching event recorded since the controller began watching fires
// it on the controller's next pass. Without this, every check of a never-run
// order re-read the whole event history, every archive included, and
// reported due on history the controller would never fire on.
func CheckEventStart(ep events.Provider, runStamps []uint64) ([]uint64, error) {
	if ep == nil || MaxStamp(runStamps) > 0 {
		return runStamps, nil
	}
	head, err := ep.LatestSeq()
	if err != nil {
		return nil, fmt.Errorf("reading event head: %w", err)
	}
	return []uint64{head}, nil
}

// listEventsWithInFlight reads the events filter matches, including those
// still stranded in an in-flight rotation file when the provider can read
// them ([events.InFlightProvider]). A plain List misses them during an
// archive's compression window, and a caller that moves its start past a
// check that matched nothing would then never see them.
func listEventsWithInFlight(ep events.Provider, filter events.Filter) ([]events.Event, error) {
	if inFlight, ok := ep.(events.InFlightProvider); ok {
		return inFlight.ListInFlight(filter)
	}
	return ep.List(filter)
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

// MaxSeqFromLabels extracts the highest seq:<N> value from bead labels,
// unbounded by any event log head: the raw high-water mark of the stamps.
func MaxSeqFromLabels(labelSets [][]string) uint64 {
	return MaxStamp(SeqsFromLabels(labelSets))
}

// MaxStamp is the highest of stamps, unbounded by any event log head; 0 when
// there are none.
func MaxStamp(stamps []uint64) uint64 {
	var highest uint64
	for _, seq := range stamps {
		highest = max(highest, seq)
	}
	return highest
}

// SeqsFromLabels extracts every valid seq:<N> value from bead labels, in
// label order: an order's run stamps, which checkEvent bounds by the event
// log's head. Labels whose value is not an unsigned integer are skipped.
func SeqsFromLabels(labelSets [][]string) []uint64 {
	var seqs []uint64
	for _, labels := range labelSets {
		for _, l := range labels {
			if strings.HasPrefix(l, "seq:") {
				if n, err := strconv.ParseUint(l[4:], 10, 64); err == nil {
					seqs = append(seqs, n)
				}
			}
		}
	}
	return seqs
}
