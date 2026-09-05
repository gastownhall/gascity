package doctor

import (
	"fmt"
	"path/filepath"
	"sort"
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

// Run counts each scheduled order's trailing consecutive failures.
//
// Unlike order-firing-current this needs no goroutine-plus-timeout guard: that
// check wraps its work because the order-history resolver opens the beads/Dolt
// store without accepting a context. This one reads only the event log.
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
	outcomes, err := readOrderOutcomeEvents(eventPath)
	if err != nil {
		result.Status = StatusError
		result.Message = fmt.Sprintf("read order outcome events: %v", err)
		return result
	}
	starts, err := controllerStartTimes(eventPath)
	if err != nil {
		result.Status = StatusError
		result.Message = fmt.Sprintf("read controller start events: %v", err)
		return result
	}

	worst := StatusOK
	monitored := 0
	failing := 0
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

		streak, lastMessage, sawOutcome, skipped := consecutiveOrderFailures(outcomes, order.ScopedName(), starts, c.grace)
		status, detail := classifyOrderOutcome(order, streak, c.threshold, lastMessage, sawOutcome, skipped)
		worst = worseStatus(worst, status)
		result.Details = append(result.Details, detail)
		if status != StatusOK {
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
	if worst == StatusOK {
		result.Message = "all scheduled orders succeeding"
	} else {
		result.Message = fmt.Sprintf("%d order(s) failing repeatedly", failing)
		result.FixHint = fmt.Sprintf(orderOutcomeInspectHintFmt, firstFailingHint)
	}
	return result
}

// readOrderOutcomeEvents returns order.completed and order.failed merged in Seq
// order. events.Filter matches a single Type, hence two reads.
func readOrderOutcomeEvents(eventPath string) ([]events.Event, error) {
	completed, err := events.ReadFiltered(eventPath, events.Filter{Type: events.OrderCompleted})
	if err != nil {
		return nil, err
	}
	failed, err := events.ReadFiltered(eventPath, events.Filter{Type: events.OrderFailed})
	if err != nil {
		return nil, err
	}
	merged := make([]events.Event, 0, len(completed)+len(failed))
	merged = append(merged, completed...)
	merged = append(merged, failed...)
	// Seq, not Ts: the log is append-only and seq-ordered, and two events in the
	// same second would otherwise sort arbitrarily.
	sort.Slice(merged, func(i, j int) bool { return merged[i].Seq < merged[j].Seq })
	return merged, nil
}

// controllerStartTimes returns every controller.started timestamp. The sibling
// check's latestControllerStartedAt returns only the newest, which is not enough
// here — see nearControllerStart.
func controllerStartTimes(eventPath string) ([]time.Time, error) {
	startEvents, err := events.ReadFiltered(eventPath, events.Filter{Type: events.ControllerStarted})
	if err != nil {
		return nil, err
	}
	out := make([]time.Time, 0, len(startEvents))
	for _, event := range startEvents {
		out = append(out, event.Ts)
	}
	return out, nil
}
