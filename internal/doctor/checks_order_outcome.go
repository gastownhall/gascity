package doctor

import (
	"cmp"
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
)

const (
	orderOutcomeHealthyName = "order-outcome-healthy"

	// orderOutcomeInspectHintFmt mirrors the sibling check's
	// orderFiringInspectHintFmt so the pair emits consistently-shaped hints.
	orderOutcomeInspectHintFmt = "Inspect with: gc order check && gc order history %s"

	// orderOutcomeFailureThreshold is the consecutive-failure count that flags an
	// order. Three, not two: two in a row is a plausible transient for anything
	// touching the network or a lock. Three, not five: on a 6h order five failures
	// is 30h before anything surfaces.
	orderOutcomeFailureThreshold = 3

	// orderOutcomeStartGrace covers gastownhall/gascity#3898 — for ~5 minutes
	// after a supervisor start, exec orders fail spuriously because dispatch
	// begins before pack staging completes. 2x margin on the observed window.
	orderOutcomeStartGrace = 10 * time.Minute
)

// nearControllerStart reports whether ts falls within grace after any controller
// start. Every start is checked, not just the newest: two restarts with no
// successful run between them would otherwise leave the older burst counted and
// manufacture a false positive.
func nearControllerStart(ts time.Time, starts []time.Time, grace time.Duration) bool {
	if grace <= 0 {
		return false
	}
	for _, start := range starts {
		if start.IsZero() || ts.Before(start) {
			continue
		}
		if ts.Sub(start) <= grace {
			return true
		}
	}
	return false
}

// consecutiveOrderFailures counts trailing order.failed events for one order.
//
// outcomes must hold order.completed and order.failed events ordered by Seq
// ascending; the walk runs newest-first and stops at the first success.
//
// A success always ends the streak, even if it falls within the post-start grace
// window. The grace window skips only spurious FAILURES (neither counting them nor
// allowing them to break the streak); a success is proof the order works.
//
// Failures inside the post-start grace window are SKIPPED, not reset. Resetting
// would let a frequently-restarting city zero a genuinely broken order's streak
// on every restart, which is the opposite of what this check is for.
//
// sawOutcome distinguishes "ran and succeeded" from "never produced an outcome";
// order-firing-current already owns the never-fired case.
//
// skipped counts trailing failures that were inside the post-start grace
// window and therefore excluded from streak. Callers need this to avoid
// reporting "last run succeeded" when every trailing run actually failed but
// was suppressed as a spurious post-restart burst — see classifyOrderOutcome.
func consecutiveOrderFailures(outcomes []events.Event, subject string, starts []time.Time, grace time.Duration) (streak int, lastMessage string, sawOutcome bool, skipped int) {
	lastMessageSet := false

	for i := len(outcomes) - 1; i >= 0; i-- {
		event := outcomes[i]
		if event.Subject != subject {
			continue
		}
		sawOutcome = true
		if event.Type != events.OrderFailed {
			break
		}
		if nearControllerStart(event.Ts, starts, grace) {
			skipped++
			continue
		}
		streak++
		if !lastMessageSet {
			lastMessage = event.Message
			lastMessageSet = true
		}
	}

	return streak, lastMessage, sawOutcome, skipped
}

// classifyOrderOutcome turns one order's failure streak into a doctor result.
//
// skipped is the grace-window-skipped-failure count from consecutiveOrderFailures.
// When streak == 0 but skipped > 0, every trailing run actually failed (just
// inside the post-start grace window); reporting "last run succeeded" would be
// a false statement in the diagnostic tool at exactly the moment an operator is
// most likely reading it — just after a restart.
func classifyOrderOutcome(order orders.Order, streak int, threshold int, lastMessage string, sawOutcome bool, skipped int) (CheckStatus, string) {
	name := orderDisplayName(order)

	if !sawOutcome {
		return StatusOK, fmt.Sprintf("%s: no completed runs yet", name)
	}
	if streak == 0 {
		if skipped > 0 {
			return StatusOK, fmt.Sprintf("%s: %d recent failure(s) within controller-start grace window", name, skipped)
		}
		return StatusOK, fmt.Sprintf("%s: last run succeeded", name)
	}
	if streak < threshold {
		return StatusOK, fmt.Sprintf("%s: %d consecutive failure(s), under threshold %d", name, streak, threshold)
	}

	detail := fmt.Sprintf("%s: %d consecutive failures", name, streak)
	if strings.TrimSpace(lastMessage) != "" {
		detail = fmt.Sprintf("%s, last %q", detail, lastMessage)
	}
	return StatusWarning, detail
}

// OrderOutcomeHealthyCheck reports scheduled orders failing repeatedly.
//
// Sibling to OrderFiringCurrentCheck, which answers "did it run?" while this
// answers "did it succeed?". An order that fires faithfully on schedule and fails
// every time leaves order-firing-current green, so the failure is invisible: the
// only trace is order.failed in the event log, which nobody reads unprompted.
type OrderOutcomeHealthyCheck struct {
	cfg       *config.City
	cityPath  string
	threshold int
	grace     time.Duration
	// scanBound, when positive, switches the check to bounded mode: it decodes
	// at most this many bytes of the active event log and reports orders it
	// cannot decide as unresolved. Zero is exact mode, the full read.
	scanBound int64
}

// OrderOutcomeOption configures the repeated-order-failure check.
type OrderOutcomeOption func(*OrderOutcomeHealthyCheck)

// WithOrderOutcomeScanBound makes the check read at most maxScanBytes of the
// newest part of the active event log, so a caller with a latency budget gets a
// verdict whatever the log's size. A bounded result may under-claim and never
// over-claims: each order is judged exactly, flagged as failing with "at least
// N" failures, or listed as unresolved (OrderOutcomePayload). Without this
// option, or with maxScanBytes <= 0, the check is exact and reads every event,
// archives included.
func WithOrderOutcomeScanBound(maxScanBytes int64) OrderOutcomeOption {
	return func(c *OrderOutcomeHealthyCheck) { c.scanBound = max(maxScanBytes, 0) }
}

// OrderOutcomePayload is the structured evidence of a bounded
// order-outcome-healthy run, projected into `gc doctor --json`. It is set only
// when the bound left orders undecided, so an exact result and a fully resolved
// bounded result carry no payload.
type OrderOutcomePayload struct {
	// Unresolved names the monitored orders the scan bound could not decide.
	// They are neither failing nor known healthy.
	Unresolved []string `json:"unresolved,omitempty"`
}

// NewOrderOutcomeHealthyCheck creates the repeated-order-failure check.
func NewOrderOutcomeHealthyCheck(cfg *config.City, cityPath string, opts ...OrderOutcomeOption) *OrderOutcomeHealthyCheck {
	c := &OrderOutcomeHealthyCheck{
		cfg:       cfg,
		cityPath:  cityPath,
		threshold: orderOutcomeFailureThreshold,
		grace:     orderOutcomeStartGrace,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Name returns the check identifier shown by gc doctor.
func (c *OrderOutcomeHealthyCheck) Name() string { return orderOutcomeHealthyName }

// CanFix reports whether the check can repair a failing order. It cannot:
// remediation depends entirely on why the order fails.
func (c *OrderOutcomeHealthyCheck) CanFix() bool { return false }

// Fix is a no-op for the reason given on CanFix.
func (c *OrderOutcomeHealthyCheck) Fix(_ *CheckContext) error { return nil }

// Run counts each scheduled order's trailing consecutive failures.
//
// Unlike order-firing-current this needs no goroutine-plus-timeout guard: that
// check wraps its work because the order-history resolver opens the beads/Dolt
// store without accepting a context. This one reads only the event log, and the
// read itself stops when the runner abandons the check.
func (c *OrderOutcomeHealthyCheck) Run(ctx *CheckContext) *CheckResult {
	// This check is advisory on every path: a failing order must not gate gc doctor.
	// Blocking would fail gc doctor outright and gate every clean-doctor dependency
	// on transient order breakage, including during maintenance. SeverityAdvisory
	// is the only source of truth, set here at construction.
	result := &CheckResult{Name: c.Name(), Severity: SeverityAdvisory}
	if c.cfg == nil {
		result.Status = StatusOK
		result.Message = "no city config loaded"
		return result
	}

	cityPath := c.cityPath
	if cityPath == "" && ctx != nil {
		cityPath = ctx.CityPath
	}
	if cityPath == "" {
		result.Status = StatusError
		result.Message = "city path unavailable"
		return result
	}

	// Same helper order-firing-current uses, so the two checks can never
	// disagree about which orders are in scope.
	allOrders, err := scanOrderFiringCurrentOrders(cityPath, c.cfg)
	if err != nil {
		result.Status = StatusError
		result.Message = fmt.Sprintf("scan orders: %v", err)
		return result
	}

	eventPath := filepath.Join(cityPath, citylayout.RuntimeRoot, "events.jsonl")
	log, err := c.readLog(ctx, eventPath)
	if err != nil {
		result.Status = StatusError
		result.Message = fmt.Sprintf("read order outcome events: %v", err)
		return result
	}

	worst := StatusOK
	monitored := 0
	failing := 0
	var unresolved []string
	var firstFailingHint string
	suspendedRigs := orderFiringCurrentSuspendedRigs(c.cfg, cityPath)

	for _, order := range allOrders {
		// Manual and event-triggered orders are out of scope by construction,
		// matching the sibling check. This is what keeps a manual order that was
		// abandoned mid-failure from alarming forever, with no recency heuristic
		// needed: it simply is not in the monitored set.
		if order.Trigger != "cron" && order.Trigger != "cooldown" {
			continue
		}
		if orderFiringCurrentOrderSuspended(suspendedRigs, order) {
			continue
		}
		monitored++

		verdict := judgeOrderOutcome(order, log, c.threshold, c.grace)
		result.Details = append(result.Details, verdict.Detail)
		if verdict.Unresolved {
			unresolved = append(unresolved, orderDisplayName(order))
			continue
		}
		worst = worseStatus(worst, verdict.Status)
		if verdict.Status != StatusOK {
			failing++
			if firstFailingHint == "" {
				// gc order history takes a bare name positionally and filters
				// on a.Name; the scoped form matches zero orders on the
				// local-iterator path used when the supervisor API is
				// unavailable — exactly when someone is debugging a broken
				// city. orderHistoryHintTarget yields the --rig form instead.
				firstFailingHint = orderHistoryHintTarget(order)
			}
		}
	}

	if monitored == 0 {
		result.Status = StatusOK
		result.Message = "no cron or cooldown orders"
		return result
	}

	result.Status = worst
	result.Message = orderOutcomeSummary(worst, failing, len(unresolved))
	if worst != StatusOK {
		result.FixHint = fmt.Sprintf(orderOutcomeInspectHintFmt, firstFailingHint)
	}
	if len(unresolved) > 0 {
		slices.Sort(unresolved)
		result.Payload = OrderOutcomePayload{Unresolved: unresolved}
	}
	return result
}

// orderOutcomeSummary is the aggregate message. With nothing unresolved it is
// the exact-mode text; an unresolved order is added to it, never folded into
// "succeeding" or "failing".
func orderOutcomeSummary(worst CheckStatus, failing, unresolved int) string {
	msg := "all scheduled orders succeeding"
	switch {
	case worst != StatusOK:
		msg = fmt.Sprintf("%d order(s) failing repeatedly", failing)
	case unresolved > 0:
		msg = "no repeated failures in the scanned window"
	}
	if unresolved > 0 {
		msg = fmt.Sprintf("%s; %d order(s) unresolved within the scan bound", msg, unresolved)
	}
	return msg
}

// readLog reads the event log the way the check was configured: the whole
// history in exact mode, a byte-bounded window of the active log otherwise.
func (c *OrderOutcomeHealthyCheck) readLog(ctx *CheckContext, eventPath string) (orderOutcomeLog, error) {
	if c.scanBound > 0 {
		return readOrderOutcomeLogBounded(ctx, eventPath, c.scanBound)
	}
	return readOrderOutcomeLog(ctx, eventPath)
}

// orderOutcomeVerdict is one order's result. Unresolved means the scan could not
// decide the order; Status is meaningless then and must not be read as OK.
type orderOutcomeVerdict struct {
	Status     CheckStatus
	Detail     string
	Unresolved bool
}

// judgeOrderOutcome decides one order from what log holds.
//
// Over a complete log it is the exact algorithm. Over a window it may only
// under-claim, so the newest-first walk returns the exact verdict when a success
// inside the window ends the streak, and otherwise stops at the first failure
// it cannot place. A failure is placed when no controller start it depends on
// can lie outside the window (see orderOutcomeLog.decidable). When the window
// ends with no success, older history may still extend the streak: at least
// threshold counted failures is a warning that exact mode confirms, and fewer
// is unresolved.
func judgeOrderOutcome(order orders.Order, log orderOutcomeLog, threshold int, grace time.Duration) orderOutcomeVerdict {
	name := order.ScopedName()
	outcomes := log.bySubject[name]
	if log.complete {
		streak, lastMessage, sawOutcome, skipped := consecutiveOrderFailures(outcomes, name, log.starts, grace)
		status, detail := classifyOrderOutcome(order, streak, threshold, lastMessage, sawOutcome, skipped)
		return orderOutcomeVerdict{Status: status, Detail: detail}
	}

	display := orderDisplayName(order)
	unresolved := orderOutcomeVerdict{Detail: fmt.Sprintf("%s: unresolved, older history not scanned", display), Unresolved: true}
	streak, skipped := 0, 0
	var lastMessage string
	for i := len(outcomes) - 1; i >= 0; i-- {
		event := outcomes[i]
		if event.Type != events.OrderFailed {
			status, detail := classifyOrderOutcome(order, streak, threshold, lastMessage, true, skipped)
			return orderOutcomeVerdict{Status: status, Detail: detail}
		}
		if !log.decidable(event.Ts, grace) {
			return unresolved
		}
		if nearControllerStart(event.Ts, log.starts, grace) {
			skipped++
			continue
		}
		if streak == 0 {
			lastMessage = event.Message
		}
		streak++
	}
	if streak < threshold {
		return unresolved
	}
	_, detail := classifyOrderOutcome(order, streak, threshold, lastMessage, true, skipped)
	return orderOutcomeVerdict{
		Status: StatusWarning,
		Detail: strings.Replace(detail, display+": ", display+": at least ", 1) + "; older history not scanned",
	}
}

// orderOutcomeLog is everything order-outcome-healthy needs from the event log,
// gathered in a single read.
type orderOutcomeLog struct {
	// bySubject holds each order's order.completed and order.failed events in
	// Seq order, keyed by the order's scoped name.
	bySubject map[string][]events.Event
	// starts holds every controller.started timestamp — see nearControllerStart.
	starts []time.Time
	// complete is true when the log is the whole history. When false it is the
	// newest window of it, and horizon is the timestamp of the oldest event that
	// window covers (zero if it covered none).
	complete bool
	horizon  time.Time
}

// decidable reports whether a failure at ts can be placed against controller
// starts using only this log. In a window, every start outside it is at or
// before the horizon, so it can be within grace of ts only if the horizon is
// itself within grace of ts. This relies on timestamps being monotone with Seq.
func (l orderOutcomeLog) decidable(ts time.Time, grace time.Duration) bool {
	if l.complete || grace <= 0 {
		return true
	}
	return !l.horizon.IsZero() && l.horizon.Before(ts.Add(-grace))
}

// readOrderOutcomeLog reads order.completed, order.failed and controller.started
// in ONE pass over the log. The log is not rotated in practice and reached
// 151MB, so each extra pass cost ~30s; the events reader screens raw lines by
// type before decoding, which is what keeps a single pass cheap.
//
// The read stops early, returning an error, once ctx reports the check
// abandoned. A nil ctx is never abandoned.
func readOrderOutcomeLog(ctx *CheckContext, eventPath string) (orderOutcomeLog, error) {
	goCtx, cancel := checkGoContext(ctx)
	defer cancel()

	evts, err := events.ReadFilteredContext(goCtx, eventPath, events.Filter{Types: orderOutcomeEventTypes})
	if err != nil {
		return orderOutcomeLog{}, err
	}
	log := groupOrderOutcomes(evts)
	log.complete = true
	return log, nil
}

// readOrderOutcomeLogBounded reads the same events from at most maxScanBytes of
// the newest part of the active log. Archives are never opened, so the result is
// complete only when that window reaches the start of the active log and no
// archive exists.
func readOrderOutcomeLogBounded(ctx *CheckContext, eventPath string, maxScanBytes int64) (orderOutcomeLog, error) {
	goCtx, cancel := checkGoContext(ctx)
	defer cancel()

	window, err := events.ReadTailWindow(goCtx, eventPath, events.Filter{Types: orderOutcomeEventTypes, MaxScanBytes: maxScanBytes})
	if err != nil {
		return orderOutcomeLog{}, err
	}
	log := groupOrderOutcomes(window.Events)
	log.complete = window.Complete
	log.horizon = window.Oldest
	return log, nil
}

var orderOutcomeEventTypes = []string{events.OrderCompleted, events.OrderFailed, events.ControllerStarted}

// groupOrderOutcomes splits events into per-order outcomes and controller
// starts, in Seq order.
func groupOrderOutcomes(evts []events.Event) orderOutcomeLog {
	// Seq, not Ts: the log is append-only and seq-ordered, and two events in the
	// same second would otherwise sort arbitrarily. The reader already yields Seq
	// order (archives oldest-first, then the active log), so this is a guard, not
	// a per-call cost.
	if !slices.IsSortedFunc(evts, compareEventSeq) {
		slices.SortStableFunc(evts, compareEventSeq)
	}

	log := orderOutcomeLog{bySubject: make(map[string][]events.Event)}
	for _, event := range evts {
		if event.Type == events.ControllerStarted {
			log.starts = append(log.starts, event.Ts)
			continue
		}
		log.bySubject[event.Subject] = append(log.bySubject[event.Subject], event)
	}
	return log
}

func compareEventSeq(a, b events.Event) int { return cmp.Compare(a.Seq, b.Seq) }

// checkGoContext adapts the runner's Done channel to a context.Context so the
// events reader can stop when the check is abandoned. The returned cancel must
// always be called to release the watcher goroutine.
//
// A check already abandoned on entry gets an already-canceled context rather
// than waiting for the watcher goroutine to be scheduled: a loaded or
// single-P scheduler can otherwise run a whole read before the cancellation
// lands.
func checkGoContext(ctx *CheckContext) (context.Context, context.CancelFunc) {
	goCtx, cancel := context.WithCancel(context.Background())
	if ctx == nil || ctx.Done == nil {
		return goCtx, cancel
	}
	select {
	case <-ctx.Done:
		cancel()
		return goCtx, cancel
	default:
	}
	go func() {
		select {
		case <-ctx.Done:
			cancel()
		case <-goCtx.Done():
		}
	}()
	return goCtx, cancel
}
