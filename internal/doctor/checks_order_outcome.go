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
// outcomes holds order.completed, order.failed and order.skipped events ordered
// by Seq ascending; the walk runs newest-first and stops at the first success.
//
// order.skipped declarations are ignored entirely: they neither count toward nor
// end the streak, and do not set sawOutcome. Every output equals the result for
// the same stream with declarations removed; latestIncompleteOrderOutcome
// reports declarations separately.
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
		if event.Subject != subject || event.Type == events.OrderSkipped {
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

// latestIncompleteOrderOutcome returns the newest order.skipped declaration
// recorded for one order after its latest order.completed.
//
// outcomes must be ordered by Seq ascending; the walk runs newest-first. A
// completion clears every older declaration; a failure neither clears nor
// hides one. Declarations are not paired with executions: the walk follows
// recorded sequence only. The boolean distinguishes a declaration with an
// empty message from no declaration at all.
func latestIncompleteOrderOutcome(outcomes []events.Event, subject string) (events.Event, bool) {
	for i := len(outcomes) - 1; i >= 0; i-- {
		event := outcomes[i]
		if event.Subject != subject {
			continue
		}
		switch event.Type {
		case events.OrderSkipped:
			return event, true
		case events.OrderCompleted:
			return events.Event{}, false
		}
	}
	return events.Event{}, false
}

// incompleteOrderOutcomeDetail renders a recorded order.skipped declaration.
// The declaration message carries the outcome, reason and skipped scopes the
// controller recorded, so it is quoted whole, with %q as the failure detail
// quotes its message: scope names can come from the bead store, and quoting
// keeps their control characters off the terminal. Declarations are not paired
// with executions, so the detail describes the recorded evidence rather than a
// run.
func incompleteOrderOutcomeDetail(order orders.Order, declaration events.Event) string {
	name := orderDisplayName(order)
	if strings.TrimSpace(declaration.Message) == "" {
		return fmt.Sprintf("%s: recorded incomplete work", name)
	}
	return fmt.Sprintf("%s: recorded incomplete work: %q", name, declaration.Message)
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

// OrderOutcomeHealthyCheck reports scheduled orders failing repeatedly, and
// reports without warning the incomplete work an order declared since its
// latest completion.
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
}

// NewOrderOutcomeHealthyCheck creates the repeated-order-failure check.
func NewOrderOutcomeHealthyCheck(cfg *config.City, cityPath string) *OrderOutcomeHealthyCheck {
	return &OrderOutcomeHealthyCheck{
		cfg:       cfg,
		cityPath:  cityPath,
		threshold: orderOutcomeFailureThreshold,
		grace:     orderOutcomeStartGrace,
	}
}

// Name returns the check identifier shown by gc doctor.
func (c *OrderOutcomeHealthyCheck) Name() string { return orderOutcomeHealthyName }

// CanFix reports whether the check can repair a failing order. It cannot:
// remediation depends entirely on why the order fails.
func (c *OrderOutcomeHealthyCheck) CanFix() bool { return false }

// Fix is a no-op for the reason given on CanFix.
func (c *OrderOutcomeHealthyCheck) Fix(_ *CheckContext) error { return nil }

// Run counts each scheduled order's trailing consecutive failures and reports
// any order.skipped declaration of incomplete work recorded since its latest
// completion. Only a failure streak warns; see reportOrderOutcome.
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
	log, err := readOrderOutcomeLog(ctx, eventPath)
	if err != nil {
		result.Status = StatusError
		result.Message = fmt.Sprintf("read order outcome events: %v", err)
		return result
	}

	var tally orderOutcomeTally
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
		tally.add(order, c.reportOrderOutcome(order, log))
	}

	if tally.monitored == 0 {
		result.Status = StatusOK
		result.Message = "no cron or cooldown orders"
		return result
	}

	result.Status = tally.worst
	result.Message = tally.message()
	result.Details = tally.details
	if tally.failing > 0 {
		result.FixHint = fmt.Sprintf(orderOutcomeInspectHintFmt, tally.hintTarget)
	}
	return result
}

// orderOutcomeReport is one monitored order's share of the check result.
type orderOutcomeReport struct {
	status  CheckStatus
	details []string
	// declared reports an order.skipped declaration recorded since the order's
	// latest completion.
	declared bool
}

// reportOrderOutcome evaluates one monitored order. Its failure streak alone
// decides the status. An outstanding declaration is listed beside trailing
// failures, or replaces the classifier's success or no-outcome detail, which
// the declaration makes false. It never warns: bundled orders declare
// by-design exclusions on every run (claims held by identities a sweep cannot
// vouch for, a store that is not a bd store), and the event log cannot tell
// those from work that keeps failing to run without a judgment call. Warning
// on every declaration would hold the check at warning for good on such
// cities and bury the failure streaks it exists to report.
func (c *OrderOutcomeHealthyCheck) reportOrderOutcome(order orders.Order, log orderOutcomeLog) orderOutcomeReport {
	subject := order.ScopedName()
	outcomes := log.bySubject[subject]
	streak, lastMessage, sawOutcome, skipped := consecutiveOrderFailures(outcomes, subject, log.starts, c.grace)
	status, detail := classifyOrderOutcome(order, streak, c.threshold, lastMessage, sawOutcome, skipped)
	report := orderOutcomeReport{status: status, details: []string{detail}}
	declaration, ok := latestIncompleteOrderOutcome(outcomes, subject)
	if !ok {
		return report
	}
	report.declared = true
	declarationDetail := incompleteOrderOutcomeDetail(order, declaration)
	if streak > 0 || skipped > 0 {
		report.details = append(report.details, declarationDetail)
	} else {
		report.details = []string{declarationDetail}
	}
	return report
}

// orderOutcomeTally accumulates the monitored orders' reports into the check
// result.
type orderOutcomeTally struct {
	monitored int
	// failing counts orders whose failure streak warns, and declaring counts
	// orders with an outstanding declaration; an order can be in both.
	failing   int
	declaring int
	// hintTarget is the first failing order's gc order history argument.
	hintTarget string
	worst      CheckStatus
	details    []string
}

// add folds one monitored order's report into the tally.
func (t *orderOutcomeTally) add(order orders.Order, report orderOutcomeReport) {
	t.monitored++
	t.worst = worseStatus(t.worst, report.status)
	t.details = append(t.details, report.details...)
	if report.declared {
		t.declaring++
	}
	if report.status == StatusOK {
		return
	}
	t.failing++
	if t.hintTarget == "" {
		// gc order history takes a bare name positionally and filters on
		// a.Name; the scoped form matches zero orders on the local-iterator
		// path used when the supervisor API is unavailable — exactly when
		// someone is debugging a broken city. orderHistoryHintTarget yields
		// the --rig form instead.
		t.hintTarget = orderHistoryHintTarget(order)
	}
}

// message summarizes the tally. Failing and declaring orders are counted
// separately, so a declaration neither hides a failure streak nor lets the
// summary claim that every order succeeded.
func (t *orderOutcomeTally) message() string {
	var summary string
	switch {
	case t.failing > 0:
		summary = fmt.Sprintf("%d order(s) failing repeatedly", t.failing)
	case t.declaring > 0:
		summary = "no order failing repeatedly"
	default:
		return "all scheduled orders succeeding"
	}
	if t.declaring > 0 {
		summary += fmt.Sprintf("; %d with recorded incomplete work", t.declaring)
	}
	return summary
}

// orderOutcomeLog is everything order-outcome-healthy needs from the event log,
// gathered in a single read.
type orderOutcomeLog struct {
	// bySubject holds each order's order.completed, order.failed and
	// order.skipped events in Seq order, keyed by the order's scoped name.
	bySubject map[string][]events.Event
	// starts holds every controller.started timestamp — see nearControllerStart.
	starts []time.Time
}

// readOrderOutcomeLog reads order.completed, order.failed, order.skipped and
// controller.started in ONE pass over the log. The log is not rotated in practice and reached
// 151MB, so each extra pass cost ~30s; the events reader screens raw lines by
// type before decoding, which is what keeps a single pass cheap.
//
// The read stops early, returning an error, once ctx reports the check
// abandoned. A nil ctx is never abandoned.
func readOrderOutcomeLog(ctx *CheckContext, eventPath string) (orderOutcomeLog, error) {
	goCtx, cancel := checkGoContext(ctx)
	defer cancel()

	evts, err := events.ReadFilteredContext(goCtx, eventPath, events.Filter{
		Types: []string{events.OrderCompleted, events.OrderFailed, events.OrderSkipped, events.ControllerStarted},
	})
	if err != nil {
		return orderOutcomeLog{}, err
	}
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
	return log, nil
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
