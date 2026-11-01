package doctor

import (
	"cmp"
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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

	// orderOutcomeEventTailLimit caps the contiguous order.completed and
	// order.failed tail one Run reads. Like order-firing-current's tail it reads
	// the newest events instead of scanning the whole log: on a busy city the
	// active log reaches hundreds of megabytes. An order whose streak the window
	// cannot settle is not lost — it is re-walked from the order-run store.
	orderOutcomeEventTailLimit = 2000

	// orderOutcomeStartTailLimit and orderOutcomeStartScanBytes bound the
	// controller.started read. Starts are rare, so without a byte budget a log
	// holding few of them degrades into a full backward walk (#4418). A start
	// outside the covered range is simply not seen, which can only make a failure
	// count where the grace window would have skipped it — and only for failures
	// older than the budget, which the outcome tail rarely reaches.
	orderOutcomeStartTailLimit = 1000
	orderOutcomeStartScanBytes = 64 << 20

	// orderOutcomeRunLimit is how many newest runs the store-backed fallback asks
	// for. The walk needs threshold counted failures; the headroom absorbs
	// in-flight runs and failures skipped inside a post-start grace window.
	orderOutcomeRunLimit = 20
)

// OrderOutcomeRecentRunsFunc lists an order's newest persisted runs, newest
// first, at most limit of them. Implementations MUST be safe for concurrent use.
type OrderOutcomeRecentRunsFunc func(order orders.Order, limit int) ([]orders.OrderRun, error)

// OrderOutcomeHealthyOption configures the repeated-order-failure check.
type OrderOutcomeHealthyOption func(*OrderOutcomeHealthyCheck)

// WithOrderOutcomeRecentRunsFunc supplies the store-backed history the check
// consults for an order its bounded event tail cannot settle.
func WithOrderOutcomeRecentRunsFunc(fn OrderOutcomeRecentRunsFunc) OrderOutcomeHealthyOption {
	return func(c *OrderOutcomeHealthyCheck) { c.recentRuns = fn }
}

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

// outcomeStep is one finished run of an order as the streak walk sees it, whether
// it came from the event log or the order-run store.
type outcomeStep struct {
	ts      time.Time
	failed  bool
	message string
}

// outcomeWalk is the result of walking an order's steps newest-first.
type outcomeWalk struct {
	streak      int
	lastMessage string
	sawOutcome  bool
	skipped     int
	// endedOnSuccess reports that the walk stopped at a success, which proves the
	// streak is complete rather than cut off by the edge of the evidence.
	endedOnSuccess bool
	// capped reports that the walk stopped at stopAt counted failures, so streak
	// is a lower bound.
	capped bool
}

// walkOutcomeSteps counts trailing failures over steps, which must be ordered
// newest-first, and stops at the first success. A positive stopAt also stops the
// walk once that many failures were counted.
//
// A success always ends the streak, even if it falls within the post-start grace
// window. The grace window skips only spurious FAILURES (neither counting them nor
// allowing them to break the streak); a success is proof the order works.
//
// Failures inside the post-start grace window are SKIPPED, not reset. Resetting
// would let a frequently-restarting city zero a genuinely broken order's streak
// on every restart, which is the opposite of what this check is for.
func walkOutcomeSteps(steps []outcomeStep, starts []time.Time, grace time.Duration, stopAt int) outcomeWalk {
	var w outcomeWalk
	lastMessageSet := false
	for _, step := range steps {
		w.sawOutcome = true
		if !step.failed {
			w.endedOnSuccess = true
			break
		}
		if nearControllerStart(step.ts, starts, grace) {
			w.skipped++
			continue
		}
		w.streak++
		if !lastMessageSet {
			w.lastMessage = step.message
			lastMessageSet = true
		}
		if stopAt > 0 && w.streak >= stopAt {
			w.capped = true
			break
		}
	}
	return w
}

// eventOutcomeSteps adapts one subject's order.completed and order.failed events
// (Seq ascending) to steps, newest first.
func eventOutcomeSteps(outcomes []events.Event, subject string) []outcomeStep {
	steps := make([]outcomeStep, 0, len(outcomes))
	for i := len(outcomes) - 1; i >= 0; i-- {
		event := outcomes[i]
		if event.Subject != subject {
			continue
		}
		steps = append(steps, outcomeStep{ts: event.Ts, failed: event.Type == events.OrderFailed, message: event.Message})
	}
	return steps
}

// hasOrderSuccess reports whether outcomes holds an order.completed for subject.
// The tail walk is uncapped, so it ends on a success exactly when one is there.
func hasOrderSuccess(outcomes []events.Event, subject string) bool {
	return slices.ContainsFunc(outcomes, func(e events.Event) bool {
		return e.Subject == subject && e.Type == events.OrderCompleted
	})
}

// runOutcomeSteps adapts an order's persisted runs (newest first) to steps. An
// in-flight run is neither a success nor a failure, so it is dropped without
// ending the streak. Runs carry no failure message; the tracking bead stores
// only the outcome.
func runOutcomeSteps(runs []orders.OrderRun) []outcomeStep {
	steps := make([]outcomeStep, 0, len(runs))
	for _, run := range runs {
		state := run.State()
		if state == "active" {
			continue
		}
		steps = append(steps, outcomeStep{ts: run.CreatedAt, failed: state == "failed"})
	}
	return steps
}

// consecutiveOrderFailures counts trailing order.failed events for one order.
//
// outcomes must hold order.completed and order.failed events ordered by Seq
// ascending; the walk runs newest-first and stops at the first success. See
// walkOutcomeSteps for how successes and the post-start grace window count.
//
// sawOutcome distinguishes "ran and succeeded" from "never produced an outcome";
// order-firing-current already owns the never-fired case.
//
// skipped counts trailing failures that were inside the post-start grace
// window and therefore excluded from streak. Callers need this to avoid
// reporting "last run succeeded" when every trailing run actually failed but
// was suppressed as a spurious post-restart burst — see classifyOrderOutcome.
func consecutiveOrderFailures(outcomes []events.Event, subject string, starts []time.Time, grace time.Duration) (streak int, lastMessage string, sawOutcome bool, skipped int) {
	w := walkOutcomeSteps(eventOutcomeSteps(outcomes, subject), starts, grace, 0)
	return w.streak, w.lastMessage, w.sawOutcome, w.skipped
}

// classifyOrderOutcome turns one order's failure streak into a doctor result.
//
// skipped is the grace-window-skipped-failure count from consecutiveOrderFailures.
// When streak == 0 but skipped > 0, every trailing run actually failed (just
// inside the post-start grace window); reporting "last run succeeded" would be
// a false statement in the diagnostic tool at exactly the moment an operator is
// most likely reading it — just after a restart.
func classifyOrderOutcome(order orders.Order, streak int, threshold int, lastMessage string, sawOutcome bool, skipped int, atLeast bool) (CheckStatus, string) {
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

	count := fmt.Sprintf("%d", streak)
	if atLeast {
		count = "at least " + count
	}
	detail := fmt.Sprintf("%s: %s consecutive failures", name, count)
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

	recentRuns     OrderOutcomeRecentRunsFunc
	historyTimeout time.Duration
	readEvents     orderFiringEventReadFunc
}

// NewOrderOutcomeHealthyCheck creates the repeated-order-failure check.
func NewOrderOutcomeHealthyCheck(cfg *config.City, cityPath string, opts ...OrderOutcomeHealthyOption) *OrderOutcomeHealthyCheck {
	check := &OrderOutcomeHealthyCheck{
		cfg:            cfg,
		cityPath:       cityPath,
		threshold:      orderOutcomeFailureThreshold,
		grace:          orderOutcomeStartGrace,
		historyTimeout: orderFiringHistoryTimeout,
		readEvents:     events.ReadFilteredTail,
	}
	for _, opt := range opts {
		opt(check)
	}
	return check
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
// Like order-firing-current it wraps its work in a goroutine-plus-timeout guard:
// an order the bounded event tail cannot settle is resolved from the order-run
// store, whose resolver opens the beads/Dolt store without accepting a context.
func (c *OrderOutcomeHealthyCheck) Run(ctx *CheckContext) *CheckResult {
	timeout := c.historyTimeout
	if timeout <= 0 {
		timeout = orderFiringHistoryTimeout
	}

	results := make(chan *CheckResult, 1)
	go func() {
		results <- c.run(ctx)
	}()

	select {
	case result := <-results:
		return result
	case <-time.After(timeout):
		// A timed-out lookup is inconclusive, not proof of failing orders; see the
		// sibling check. Advisory and TimedOut keep it from gating gc doctor.
		return &CheckResult{
			Name:     c.Name(),
			Status:   StatusError,
			Severity: SeverityAdvisory,
			TimedOut: true,
			Message:  fmt.Sprintf("order outcome history lookup timed out after %s", timeout),
			FixHint:  orderFiringTimeoutHint,
		}
	}
}

// outcomeVerdict is one monitored order's failure streak, from the event tail or
// from the order-run store.
type outcomeVerdict struct {
	order orders.Order
	walk  outcomeWalk
	err   error
}

func (c *OrderOutcomeHealthyCheck) run(ctx *CheckContext) *CheckResult {
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
	log, err := readOrderOutcomeLogUsing(ctx, eventPath, c.readEventTail)
	if err != nil {
		result.Status = StatusError
		result.Message = fmt.Sprintf("read order outcome events: %v", err)
		return result
	}

	suspendedRigs := orderFiringCurrentSuspendedRigs(c.cfg, cityPath)
	var verdicts []outcomeVerdict
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
		scoped := order.ScopedName()
		streak, lastMessage, sawOutcome, skipped := consecutiveOrderFailures(log.bySubject[scoped], scoped, log.starts, c.grace)
		verdicts = append(verdicts, outcomeVerdict{order: order, walk: outcomeWalk{
			streak:         streak,
			lastMessage:    lastMessage,
			sawOutcome:     sawOutcome,
			skipped:        skipped,
			endedOnSuccess: hasOrderSuccess(log.bySubject[scoped], scoped),
		}})
	}
	c.resolveInsufficient(verdicts, log.starts)

	worst := StatusOK
	failing := 0
	unreadable := 0
	var firstFailingHint string
	for _, v := range verdicts {
		var status CheckStatus
		var detail string
		if v.err != nil {
			status = StatusError
			detail = fmt.Sprintf("%s: cannot read order history: %v", orderDisplayName(v.order), v.err)
			unreadable++
		} else {
			status, detail = classifyOrderOutcome(v.order, v.walk.streak, c.threshold, v.walk.lastMessage, v.walk.sawOutcome, v.walk.skipped, v.walk.capped)
			if status != StatusOK {
				failing++
			}
		}
		worst = worseStatus(worst, status)
		result.Details = append(result.Details, detail)
		if status != StatusOK && firstFailingHint == "" {
			// gc order history takes a bare name positionally and filters
			// on a.Name; the scoped form matches zero orders on the
			// local-iterator path used when the supervisor API is
			// unavailable — exactly when someone is debugging a broken
			// city. orderHistoryHintTarget yields the --rig form instead.
			firstFailingHint = orderHistoryHintTarget(v.order)
		}
	}

	if len(verdicts) == 0 {
		result.Status = StatusOK
		result.Message = "no cron or cooldown orders"
		return result
	}

	result.Status = worst
	var problems []string
	if failing > 0 {
		problems = append(problems, fmt.Sprintf("%d order(s) failing repeatedly", failing))
	}
	if unreadable > 0 {
		problems = append(problems, fmt.Sprintf("%d order(s) with unreadable history", unreadable))
	}
	if len(problems) == 0 {
		result.Message = "all scheduled orders succeeding"
	} else {
		result.Message = strings.Join(problems, "; ")
		result.FixHint = fmt.Sprintf(orderOutcomeInspectHintFmt, firstFailingHint)
	}
	return result
}

// outcomeSufficient reports whether the walk settled the order's streak from the
// evidence it had: it ended on a success, or counted the threshold's worth of
// failures. Anything else — including no outcome at all — may be an older success
// or failure sitting just past the window, so the event tail cannot decide.
func (c *OrderOutcomeHealthyCheck) outcomeSufficient(w outcomeWalk) bool {
	return w.endedOnSuccess || w.streak >= c.threshold
}

// resolveInsufficient replaces the verdict of every order whose event-tail walk
// is not sufficient with a walk over its newest persisted runs. The replacement
// is total, never merged with the tail: the two sources count different things
// (an event can outlive its run bead and vice versa), and splicing them would
// let a stale event bridge a gap in the store's history. With no fallback
// configured the tail verdict stands.
//
// Lookups are store round-trips, so they run in parallel up to
// orderFiringLastRunConcurrency; a slow serial pass is what pushes a busy city
// past the check budget. Results, errors included, land on the verdicts in
// place, so the classification loop never calls the store itself.
func (c *OrderOutcomeHealthyCheck) resolveInsufficient(verdicts []outcomeVerdict, starts []time.Time) {
	if c.recentRuns == nil {
		return
	}
	var pending []int
	for i, v := range verdicts {
		if !c.outcomeSufficient(v.walk) {
			pending = append(pending, i)
		}
	}
	if len(pending) == 0 {
		return
	}

	limit := min(len(pending), orderFiringLastRunConcurrency)
	var wg sync.WaitGroup
	sem := make(chan struct{}, limit)
	for _, i := range pending {
		wg.Add(1)
		go func(v *outcomeVerdict) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			runs, err := c.recentRuns(v.order, orderOutcomeRunLimit)
			if err != nil {
				v.err = err
				return
			}
			v.walk = walkOutcomeSteps(runOutcomeSteps(runs), starts, c.grace, c.threshold)
		}(&verdicts[i])
	}
	wg.Wait()
}

// readEventTail reads the tail of the city event log through the check's
// reader, defaulting to the real bounded reader when none was injected.
func (c *OrderOutcomeHealthyCheck) readEventTail(path string, filter events.Filter, limit int) ([]events.Event, error) {
	read := c.readEvents
	if read == nil {
		read = events.ReadFilteredTail
	}
	return read(path, filter, limit)
}

// orderOutcomeLog is everything order-outcome-healthy needs from the event log.
type orderOutcomeLog struct {
	// bySubject holds each order's order.completed and order.failed events in
	// Seq order, keyed by the order's scoped name.
	bySubject map[string][]events.Event
	// starts holds every controller.started timestamp — see nearControllerStart.
	starts []time.Time
}

// readOrderOutcomeLog reads the order outcome tail and controller starts from
// the active event log. See readOrderOutcomeLogUsing.
func readOrderOutcomeLog(ctx *CheckContext, eventPath string) (orderOutcomeLog, error) {
	return readOrderOutcomeLogUsing(ctx, eventPath, events.ReadFilteredTail)
}

// readOrderOutcomeLogUsing reads, through read, the newest
// orderOutcomeEventTailLimit order.completed and order.failed events as ONE
// contiguous window — separate per-type tails would leave gaps inside a streak —
// and, as a second bounded read, the controller.started events.
//
// The log is not rotated in practice and reached 151MB, so an unbounded scan cost
// ~30s; a tail read stops as soon as its window is full. Only the active file is
// read: an order whose streak the window cannot settle is resolved from the
// order-run store instead (see resolveInsufficient).
//
// The tail reader takes no context, so the abandoned-check test happens before
// each read. A nil ctx is never abandoned.
func readOrderOutcomeLogUsing(ctx *CheckContext, eventPath string, read orderFiringEventReadFunc) (orderOutcomeLog, error) {
	goCtx, cancel := checkGoContext(ctx)
	defer cancel()

	if err := goCtx.Err(); err != nil {
		return orderOutcomeLog{}, err
	}
	evts, err := read(eventPath, events.Filter{
		Types: []string{events.OrderCompleted, events.OrderFailed},
	}, orderOutcomeEventTailLimit)
	if err != nil {
		return orderOutcomeLog{}, err
	}
	if err := goCtx.Err(); err != nil {
		return orderOutcomeLog{}, err
	}
	startEvents, err := read(eventPath, events.Filter{
		Type:         events.ControllerStarted,
		MaxScanBytes: orderOutcomeStartScanBytes,
	}, orderOutcomeStartTailLimit)
	if err != nil {
		return orderOutcomeLog{}, err
	}
	// Seq, not Ts: the log is append-only and seq-ordered, and two events in the
	// same second would otherwise sort arbitrarily. The tail reader already yields
	// Seq order, so this is a guard, not a per-call cost.
	if !slices.IsSortedFunc(evts, compareEventSeq) {
		slices.SortStableFunc(evts, compareEventSeq)
	}
	if !slices.IsSortedFunc(startEvents, compareEventSeq) {
		slices.SortStableFunc(startEvents, compareEventSeq)
	}

	log := orderOutcomeLog{bySubject: make(map[string][]events.Event)}
	for _, event := range startEvents {
		log.starts = append(log.starts, event.Ts)
	}
	for _, event := range evts {
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
