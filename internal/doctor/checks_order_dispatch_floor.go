package doctor

import (
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/orders"
)

const orderDispatchFloorName = "order-dispatch-floor"

// OrderDispatchFloorInapplicableFunc reports whether an order's
// requires_backend rules out the backend its bead scope uses. The dispatcher
// never runs such an order there, so it does not occupy a dispatch slot.
type OrderDispatchFloorInapplicableFunc func(order orders.Order) (backend string, inapplicable bool)

// OrderDispatchFloorOption configures the dispatch-floor check.
type OrderDispatchFloorOption func(*OrderDispatchFloorCheck)

// WithOrderDispatchFloorInapplicableFunc lets callers drop orders that do not
// apply to their scope's backend from the dispatch ring.
func WithOrderDispatchFloorInapplicableFunc(fn OrderDispatchFloorInapplicableFunc) OrderDispatchFloorOption {
	return func(c *OrderDispatchFloorCheck) {
		c.inapplicable = fn
	}
}

// OrderDispatchFloorCheck reports cooldown and cron orders whose configured
// interval is shorter than the dispatcher can achieve.
//
// The dispatcher starts at most orders.max_dispatches_per_tick orders per pass
// and passes run one per patrol interval, so a ring of N cooldown, cron and
// event orders takes ceil(N/budget) passes to come all the way round. When
// every counted order is due together, an order can fire no more often than
// that: a 30s cooldown beside eight other due orders fires about every 90s,
// and order-firing-current later reports it stale with no hint that the
// config can never be met.
//
// N counts every enabled cooldown, cron and event order, not just the ones due
// on a given pass. That is an upper bound, and it needs no store reads: a
// config-time check cannot know which orders will be due together. Condition
// orders are left out because the dispatcher starts due ones outside the
// budget; manual and webhook orders are never dispatched on a pass.
type OrderDispatchFloorCheck struct {
	cfg          *config.City
	cityPath     string
	inapplicable OrderDispatchFloorInapplicableFunc
}

// NewOrderDispatchFloorCheck creates the dispatch-floor check.
func NewOrderDispatchFloorCheck(cfg *config.City, cityPath string, opts ...OrderDispatchFloorOption) *OrderDispatchFloorCheck {
	check := &OrderDispatchFloorCheck{cfg: cfg, cityPath: cityPath}
	for _, opt := range opts {
		opt(check)
	}
	return check
}

// Name returns the check identifier shown by gc doctor.
func (c *OrderDispatchFloorCheck) Name() string { return orderDispatchFloorName }

// CanFix reports whether the check can repair the finding. It cannot: raising
// the budget, lengthening an interval or suspending orders is the operator's
// call.
func (c *OrderDispatchFloorCheck) CanFix() bool { return false }

// Fix is a no-op for the reason given on CanFix.
func (c *OrderDispatchFloorCheck) Fix(_ *CheckContext) error { return nil }

// Run compares each cooldown and cron order's interval to the dispatch floor.
func (c *OrderDispatchFloorCheck) Run(ctx *CheckContext) *CheckResult {
	// Advisory on every path, like order-outcome-healthy: a configuration
	// observation must not gate gc doctor. A finding is a warning, never an
	// error: the ring size is an upper bound on the orders due together, and
	// a stock city's own 30s core orders sit well below its floor.
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

	// Same scan order-firing-current uses, so the two checks agree on which
	// orders exist.
	allOrders, err := scanOrderFiringCurrentOrders(cityPath, c.cfg)
	if err != nil {
		result.Status = StatusError
		result.Message = fmt.Sprintf("scan orders: %v", err)
		return result
	}

	suspendedRigs := orderFiringCurrentSuspendedRigs(c.cfg, cityPath)
	var ring []orders.Order
	for _, order := range allOrders {
		switch order.Trigger {
		case "cooldown", "cron", "event":
		default:
			continue
		}
		if orderFiringCurrentOrderSuspended(suspendedRigs, order) {
			continue
		}
		if c.inapplicable != nil {
			if _, ok := c.inapplicable(order); ok {
				continue
			}
		}
		ring = append(ring, order)
	}

	budget := c.cfg.Orders.EffectiveMaxDispatchesPerTick()
	patrol := c.cfg.Daemon.PatrolIntervalDuration()
	passes := (len(ring) + budget - 1) / budget
	if passes < 1 {
		passes = 1
	}
	floor := time.Duration(passes) * patrol

	cronCache := make(map[string]time.Duration)
	flagged := 0
	var tightest time.Duration
	for _, order := range ring {
		if order.Trigger == "event" {
			continue
		}
		// An interval that cannot be computed is order-firing-current's to
		// report.
		interval, err := expectedIntervalForOrder(order, cronCache)
		if err != nil || interval >= floor {
			continue
		}
		flagged++
		if tightest == 0 || interval < tightest {
			tightest = interval
		}
		result.Details = append(result.Details, fmt.Sprintf("%s: %s interval %s is below the %s dispatch floor",
			orderDisplayName(order), order.Trigger, formatDispatchFloorDuration(interval), formatDispatchFloorDuration(floor)))
	}

	if flagged == 0 {
		result.Status = StatusOK
		result.Message = fmt.Sprintf("no order interval below the %s dispatch floor", formatDispatchFloorDuration(floor))
		return result
	}

	result.Details = append(result.Details, fmt.Sprintf("ring: %d order(s) dispatched %d per pass, one pass per %s patrol, %d pass(es) to come round",
		len(ring), budget, formatDispatchFloorDuration(patrol), passes))
	result.Status = StatusWarning
	result.Message = fmt.Sprintf("%d order(s) set to fire more often than the %s dispatch floor (%d orders in the ring, %d dispatched per %s pass)",
		flagged, formatDispatchFloorDuration(floor), len(ring), budget, formatDispatchFloorDuration(patrol))
	result.FixHint = orderDispatchFloorHint(len(ring), patrol, tightest, floor)
	return result
}

// orderDispatchFloorHint names the levers: the budget that would bring the
// floor down to the tightest flagged interval when one exists, the interval
// itself, and the ring.
func orderDispatchFloorHint(ringSize int, patrol, tightest, floor time.Duration) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Worst case, with every counted order due together, the minimum interval is %s. ", formatDispatchFloorDuration(floor))
	if allowed := int(tightest / patrol); allowed >= 1 {
		fmt.Fprintf(&b, "Raise orders.max_dispatches_per_tick to %d to fit the tightest order, ", (ringSize+allowed-1)/allowed)
	} else {
		fmt.Fprintf(&b, "The tightest interval is shorter than the %s patrol, so a budget cannot fit it: lower daemon.patrol_interval, ", formatDispatchFloorDuration(patrol))
	}
	b.WriteString("lengthen the flagged intervals to at least that estimate, or suspend orders to shrink the ring.")
	return b.String()
}

// formatDispatchFloorDuration prints d at second granularity: unlike
// formatOrderFiringDuration, which rounds to minutes, a 30s patrol and a 90s
// floor have to read as themselves.
func formatDispatchFloorDuration(d time.Duration) string {
	s := d.Round(time.Second).String()
	if strings.HasSuffix(s, "m0s") {
		s = s[:len(s)-2]
	}
	if strings.HasSuffix(s, "h0m") {
		s = s[:len(s)-2]
	}
	return s
}
