package doctor

import (
	"fmt"
	"log"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orderdiscovery"
	"github.com/gastownhall/gascity/internal/orders"
)

const (
	orderFiringCurrentName    = "order-firing-current"
	orderFiringInspectHintFmt = "Inspect with: gc order check && gc order history %s"
)

// OrderFiringCurrentLastRunFunc reports the newest persisted run time for an order.
type OrderFiringCurrentLastRunFunc func(order orders.Order) (time.Time, error)

// OrderFiringCurrentOption configures the scheduled-order freshness check.
type OrderFiringCurrentOption func(*OrderFiringCurrentCheck)

// WithOrderFiringCurrentLastRunFunc lets callers provide the same order-run
// history source used by `gc order history` so doctor can classify manual runs.
func WithOrderFiringCurrentLastRunFunc(fn OrderFiringCurrentLastRunFunc) OrderFiringCurrentOption {
	return func(c *OrderFiringCurrentCheck) {
		c.lastRun = fn
	}
}

// OrderFiringCurrentCheck reports scheduled orders whose last firing is stale.
type OrderFiringCurrentCheck struct {
	cfg      *config.City
	cityPath string
	clock    func() time.Time
	lastRun  OrderFiringCurrentLastRunFunc
}

// NewOrderFiringCurrentCheck creates a check for cron and cooldown order freshness.
func NewOrderFiringCurrentCheck(cfg *config.City, cityPath string, opts ...OrderFiringCurrentOption) *OrderFiringCurrentCheck {
	check := &OrderFiringCurrentCheck{
		cfg:      cfg,
		cityPath: cityPath,
		clock:    time.Now,
	}
	for _, opt := range opts {
		opt(check)
	}
	return check
}

// Name returns the check identifier shown by gc doctor.
func (c *OrderFiringCurrentCheck) Name() string { return orderFiringCurrentName }

// CanFix reports whether the check can repair stale order firing state.
func (c *OrderFiringCurrentCheck) CanFix() bool { return false }

// Fix is a no-op because stale order remediation depends on the root cause.
func (c *OrderFiringCurrentCheck) Fix(_ *CheckContext) error { return nil }

// Run compares each cron or cooldown order with its order.fired history.
func (c *OrderFiringCurrentCheck) Run(ctx *CheckContext) *CheckResult {
	result := &CheckResult{Name: c.Name()}
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

	allOrders, err := scanOrderFiringCurrentOrders(cityPath, c.cfg)
	if err != nil {
		result.Status = StatusError
		result.Message = fmt.Sprintf("scan orders: %v", err)
		return result
	}

	eventPath := filepath.Join(cityPath, citylayout.RuntimeRoot, "events.jsonl")
	firedEvents, err := events.ReadFiltered(eventPath, events.Filter{Type: events.OrderFired})
	if err != nil {
		result.Status = StatusError
		result.Message = fmt.Sprintf("read order firing events: %v", err)
		return result
	}
	startedAt, err := latestControllerStartedAt(eventPath)
	if err != nil {
		result.Status = StatusError
		result.Message = fmt.Sprintf("read controller start events: %v", err)
		return result
	}

	now := c.clock()
	if now.IsZero() {
		now = time.Now()
	}
	cronIntervals := map[string]time.Duration{}
	worst := StatusOK
	monitored := 0
	var firstNonOK string
	// Track severity contributions across error-level entries. Warnings should
	// stay visible without converting an advisory error into a blocking gate.
	var blockingErrors, advisoryErrors int
	suspendedRigs := orderFiringCurrentSuspendedRigs(c.cfg)

	for _, order := range allOrders {
		if order.Trigger != "cron" && order.Trigger != "cooldown" {
			continue
		}
		if orderFiringCurrentOrderSuspended(suspendedRigs, order) {
			continue
		}
		monitored++
		expected, err := expectedIntervalForOrder(order, cronIntervals)
		if err != nil {
			worst = worseStatus(worst, StatusError)
			result.Details = append(result.Details, fmt.Sprintf("%s: cannot compute expected interval: %v", orderDisplayName(order), err))
			if firstNonOK == "" {
				firstNonOK = orderHistoryHintTarget(order)
			}
			blockingErrors++
			continue
		}
		lastFired, err := c.latestOrderFiredAt(firedEvents, order, expected, now)
		if err != nil {
			worst = worseStatus(worst, StatusError)
			result.Details = append(result.Details, fmt.Sprintf("%s: cannot read order history: %v", orderDisplayName(order), err))
			if firstNonOK == "" {
				firstNonOK = orderHistoryHintTarget(order)
			}
			blockingErrors++
			continue
		}
		status, severity, detail := classifyOrderFiring(order, now, expected, lastFired, startedAt)
		worst = worseStatus(worst, status)
		result.Details = append(result.Details, detail)
		if status != StatusOK {
			if firstNonOK == "" {
				firstNonOK = orderHistoryHintTarget(order)
			}
			if status == StatusError {
				if severity == SeverityBlocking {
					blockingErrors++
				} else {
					advisoryErrors++
				}
			}
		}
	}

	if monitored == 0 {
		result.Status = StatusOK
		result.Message = "no cron or cooldown orders"
		return result
	}

	result.Status = worst
	switch worst {
	case StatusOK:
		result.Message = "all scheduled orders are current"
	case StatusWarning:
		result.Message = "scheduled orders are overdue"
	case StatusError:
		result.Message = "scheduled orders are stale"
	}
	if blockingErrors == 0 && advisoryErrors > 0 {
		result.Severity = SeverityAdvisory
	}
	if firstNonOK != "" {
		result.FixHint = fmt.Sprintf(orderFiringInspectHintFmt, firstNonOK)
	}
	return result
}

func scanOrderFiringCurrentOrders(cityPath string, cfg *config.City) ([]orders.Order, error) {
	scanCfg := orderFiringCurrentScanConfig(cfg)
	scanCfg = orderFiringCurrentPruneSuspendedOnlyWildcardOverrides(cityPath, cfg, scanCfg)
	allOrders, err := orderdiscovery.ScanAll(cityPath, scanCfg, orderFiringCurrentScanOptions(cityPath))
	if err != nil {
		return nil, err
	}
	return orders.FilterEnabled(allOrders), nil
}

func orderFiringCurrentScanOptions(cityPath string) orderdiscovery.ScanOptions {
	return orderdiscovery.ScanOptions{
		OnValidateError: func(orderName string, err error) error {
			log.Printf("gc doctor: skipping invalid order %s for %s: %v", orderName, cityPath, err)
			return nil
		},
		ValidateOrder: orders.ValidateExecEnvOverrides,
	}
}

func orderFiringCurrentScanConfig(cfg *config.City) *config.City {
	if cfg == nil {
		return nil
	}
	suspended := orderFiringCurrentSuspendedRigs(cfg)
	if len(suspended) == 0 {
		return cfg
	}
	clone := *cfg
	if len(cfg.FormulaLayers.Rigs) > 0 {
		clone.FormulaLayers.Rigs = make(map[string][]string, len(cfg.FormulaLayers.Rigs))
		for rigName, layers := range cfg.FormulaLayers.Rigs {
			if suspended[rigName] {
				continue
			}
			clone.FormulaLayers.Rigs[rigName] = layers
		}
	}
	if len(cfg.RigPackDirs) > 0 {
		clone.RigPackDirs = make(map[string][]string, len(cfg.RigPackDirs))
		for rigName, dirs := range cfg.RigPackDirs {
			if suspended[rigName] {
				continue
			}
			clone.RigPackDirs[rigName] = dirs
		}
	}
	if len(cfg.Orders.Overrides) > 0 {
		clone.Orders.Overrides = make([]config.OrderOverride, 0, len(cfg.Orders.Overrides))
		for _, override := range cfg.Orders.Overrides {
			if suspended[strings.TrimSpace(override.Rig)] {
				continue
			}
			clone.Orders.Overrides = append(clone.Orders.Overrides, override)
		}
	}
	return &clone
}

func orderFiringCurrentPruneSuspendedOnlyWildcardOverrides(cityPath string, originalCfg, scanCfg *config.City) *config.City {
	if originalCfg == nil || scanCfg == nil || len(scanCfg.Orders.Overrides) == 0 {
		return scanCfg
	}
	suspended := orderFiringCurrentSuspendedRigs(originalCfg)
	if len(suspended) == 0 {
		return scanCfg
	}
	activeOrders, err := orderFiringCurrentScanWithoutOverrides(cityPath, scanCfg)
	if err != nil {
		return scanCfg
	}
	allOrders, err := orderFiringCurrentScanWithoutOverrides(cityPath, originalCfg)
	if err != nil {
		return scanCfg
	}
	activeNames := map[string]bool{}
	for _, order := range activeOrders {
		activeNames[order.Name] = true
	}
	suspendedOnlyNames := map[string]bool{}
	for _, order := range allOrders {
		if order.Name == "" || !suspended[order.Rig] || activeNames[order.Name] {
			continue
		}
		suspendedOnlyNames[order.Name] = true
	}
	if len(suspendedOnlyNames) == 0 {
		return scanCfg
	}
	clone := *scanCfg
	clone.Orders.Overrides = make([]config.OrderOverride, 0, len(scanCfg.Orders.Overrides))
	for _, override := range scanCfg.Orders.Overrides {
		if strings.TrimSpace(override.Rig) == orders.RigWildcard && suspendedOnlyNames[strings.TrimSpace(override.Name)] {
			continue
		}
		clone.Orders.Overrides = append(clone.Orders.Overrides, override)
	}
	return &clone
}

func orderFiringCurrentScanWithoutOverrides(cityPath string, cfg *config.City) ([]orders.Order, error) {
	if cfg == nil {
		return orderdiscovery.ScanAll(cityPath, nil, orderFiringCurrentScanOptions(cityPath))
	}
	clone := *cfg
	clone.Orders.Overrides = nil
	return orderdiscovery.ScanAll(cityPath, &clone, orderFiringCurrentScanOptions(cityPath))
}

func orderFiringCurrentSuspendedRigs(cfg *config.City) map[string]bool {
	out := make(map[string]bool)
	if cfg == nil {
		return out
	}
	for _, rig := range cfg.Rigs {
		if rig.Suspended && strings.TrimSpace(rig.Name) != "" {
			out[rig.Name] = true
		}
	}
	return out
}

func orderFiringCurrentOrderSuspended(suspended map[string]bool, order orders.Order) bool {
	if suspended[order.Rig] {
		return true
	}
	// Defensive support for legacy qualified pool values. Bare pool names parse
	// with an empty rig and intentionally do not imply suspension by themselves.
	if rigName, _ := config.ParseQualifiedName(order.Pool); rigName != "" && suspended[rigName] {
		return true
	}
	return false
}

func expectedIntervalForOrder(order orders.Order, cronCache map[string]time.Duration) (time.Duration, error) {
	switch order.Trigger {
	case "cooldown":
		interval, err := time.ParseDuration(order.Interval)
		if err != nil {
			return 0, fmt.Errorf("parse cooldown interval %q: %w", order.Interval, err)
		}
		if interval <= 0 {
			return 0, fmt.Errorf("cooldown interval %q must be positive", order.Interval)
		}
		return interval, nil
	case "cron":
		if cached, ok := cronCache[order.Schedule]; ok {
			return cached, nil
		}
		interval, err := computeExpectedIntervalForCronSchedule(order.Schedule)
		if err != nil {
			return 0, err
		}
		cronCache[order.Schedule] = interval
		return interval, nil
	default:
		return 0, fmt.Errorf("unsupported trigger %q", order.Trigger)
	}
}

func computeExpectedIntervalForCronSchedule(schedule string) (time.Duration, error) {
	fields := strings.Fields(schedule)
	if len(fields) != 5 {
		return 0, fmt.Errorf("invalid cron schedule: want 5 fields, got %d", len(fields))
	}

	// Scan minute-by-minute from a fixed base so the result is deterministic
	// and independent of when the check runs. Widen the scan progressively so
	// weekly, monthly, and yearly schedules are computed honestly instead of
	// erroring out: the typical 24h window has zero matches for any schedule
	// coarser than daily (#2499). The 24h fast-path stays cheap for the
	// common case; coarser schedules pay the larger scan once per unique
	// schedule (results are cached at the caller).
	//
	// Base is the start of a leap year so the 366d window can include a
	// Feb 29 occurrence — `0 0 29 2 *` (leap-day schedules) would otherwise
	// produce a permanent doctor-red on cities whose check started outside
	// a leap-year window (Copilot review on #2525).
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	windowsMinutes := []int{
		1440,       // 24h — covers sub-daily and daily schedules
		7 * 1440,   // 7d  — covers weekly and weekday-set schedules
		31 * 1440,  // 31d — covers monthly schedules (longest month)
		366 * 1440, // 366d — covers yearly + leap-year (Feb 29) schedules
	}
	lastWindowIndex := len(windowsMinutes) - 1
	for windowIndex, windowMinutes := range windowsMinutes {
		matches := make([]time.Time, 0, 16)
		for i := 0; i < windowMinutes; i++ {
			ts := base.Add(time.Duration(i) * time.Minute)
			matched, err := cronScheduleMatchesAt(fields, ts)
			if err != nil {
				return 0, err
			}
			if matched {
				matches = append(matches, ts)
			}
		}
		if len(matches) == 0 {
			continue
		}
		window := time.Duration(windowMinutes) * time.Minute
		if len(matches) == 1 {
			// Don't fix the interval on the first window that happens to
			// catch one match: a yearly schedule whose firing minute
			// coincidentally falls inside the 24h or 7d window (e.g.
			// `0 0 12 5 *` from a base near May 5) would otherwise be
			// mis-classified as sub-daily. Keep widening until either a
			// second match lands (use the real minGap) or we exhaust the
			// horizon — only then is the window length a defensible
			// conservative interval (Copilot review on #2525).
			if windowIndex < lastWindowIndex {
				continue
			}
			return window, nil
		}
		minGap := window
		for i := 1; i < len(matches); i++ {
			gap := matches[i].Sub(matches[i-1])
			if gap < minGap {
				minGap = gap
			}
		}
		// Do not include a wrap-around gap (matches[0]+window - matches[last]).
		// It is only meaningful when the schedule's natural period divides the
		// window evenly, and produces wrong results for schedules whose period
		// does not — e.g. a weekly schedule in the 31d window would report a
		// bogus 3d "wrap" from Mon to Mon-of-next-month-mod-31d, drowning out
		// the real 7d gap from the loop above.
		return minGap, nil
	}
	return 0, fmt.Errorf("cron schedule %q has no firing minutes in a 366-day window", schedule)
}

func cronScheduleMatchesAt(fields []string, ts time.Time) (bool, error) {
	specs := []struct {
		name     string
		field    string
		value    int
		min, max int
	}{
		{name: "minute", field: fields[0], value: ts.Minute(), min: 0, max: 59},
		{name: "hour", field: fields[1], value: ts.Hour(), min: 0, max: 23},
		{name: "day-of-month", field: fields[2], value: ts.Day(), min: 1, max: 31},
		{name: "month", field: fields[3], value: int(ts.Month()), min: 1, max: 12},
		{name: "day-of-week", field: fields[4], value: int(ts.Weekday()), min: 0, max: 6},
	}
	for _, spec := range specs {
		matched, err := cronFieldMatchesForDoctor(spec.field, spec.value, spec.min, spec.max)
		if err != nil {
			return false, fmt.Errorf("invalid cron schedule: cannot parse %s field %q", spec.name, spec.field)
		}
		if !matched {
			return false, nil
		}
	}
	return true, nil
}

func cronFieldMatchesForDoctor(field string, value, lowerBound, upperBound int) (bool, error) {
	if strings.TrimSpace(field) == "" {
		return false, fmt.Errorf("empty field")
	}
	for _, rawPart := range strings.Split(field, ",") {
		part := strings.TrimSpace(rawPart)
		matched, err := cronPartMatchesForDoctor(part, value, lowerBound, upperBound)
		if err != nil {
			return false, err
		}
		if matched {
			return true, nil
		}
	}
	return false, nil
}

func cronPartMatchesForDoctor(part string, value, lowerBound, upperBound int) (bool, error) {
	if part == "" {
		return false, fmt.Errorf("empty part")
	}
	rangePart, stepPart, hasStep := strings.Cut(part, "/")
	step := 1
	if hasStep {
		parsed, err := strconv.Atoi(strings.TrimSpace(stepPart))
		if err != nil || parsed <= 0 {
			return false, fmt.Errorf("invalid step")
		}
		step = parsed
	}

	lo, hi, err := cronRangeForDoctor(strings.TrimSpace(rangePart), lowerBound, upperBound)
	if err != nil {
		return false, err
	}
	if value < lo || value > hi {
		return false, nil
	}
	return (value-lo)%step == 0, nil
}

func cronRangeForDoctor(rangePart string, lowerBound, upperBound int) (int, int, error) {
	switch {
	case rangePart == "*":
		return lowerBound, upperBound, nil
	case strings.Contains(rangePart, "-"):
		start, end, ok := strings.Cut(rangePart, "-")
		if !ok {
			return 0, 0, fmt.Errorf("invalid range")
		}
		lo, err := strconv.Atoi(strings.TrimSpace(start))
		if err != nil {
			return 0, 0, err
		}
		hi, err := strconv.Atoi(strings.TrimSpace(end))
		if err != nil {
			return 0, 0, err
		}
		if lo < lowerBound || hi > upperBound || lo > hi {
			return 0, 0, fmt.Errorf("range out of bounds")
		}
		return lo, hi, nil
	default:
		value, err := strconv.Atoi(rangePart)
		if err != nil {
			return 0, 0, err
		}
		if value < lowerBound || value > upperBound {
			return 0, 0, fmt.Errorf("value out of bounds")
		}
		return value, value, nil
	}
}

func latestControllerStartedAt(eventPath string) (time.Time, error) {
	startEvents, err := events.ReadFiltered(eventPath, events.Filter{Type: events.ControllerStarted})
	if err != nil {
		return time.Time{}, err
	}
	var latest time.Time
	for _, event := range startEvents {
		if event.Ts.After(latest) {
			latest = event.Ts
		}
	}
	return latest, nil
}

func (c *OrderFiringCurrentCheck) latestOrderFiredAt(evts []events.Event, order orders.Order, expected time.Duration, now time.Time) (time.Time, error) {
	latest := latestOrderFiredAt(evts, order.ScopedName())
	if c.lastRun == nil {
		return latest, nil
	}
	if !latest.IsZero() && now.Sub(latest) < expected+expected/2 {
		return latest, nil
	}
	runAt, err := c.lastRun(order)
	if err != nil {
		return time.Time{}, err
	}
	if runAt.After(latest) {
		return runAt, nil
	}
	return latest, nil
}

func latestOrderFiredAt(evts []events.Event, subject string) time.Time {
	var latest time.Time
	for _, event := range evts {
		if event.Subject != subject {
			continue
		}
		if event.Ts.After(latest) {
			latest = event.Ts
		}
	}
	return latest
}

func classifyOrderFiring(order orders.Order, now time.Time, expected time.Duration, lastFired, controllerStarted time.Time) (CheckStatus, CheckSeverity, string) {
	name := orderDisplayName(order)
	if lastFired.IsZero() {
		if controllerStarted.IsZero() {
			return StatusOK, SeverityBlocking, fmt.Sprintf("%s: never fired (controller start unknown)", name)
		}
		uptime := nonNegativeDuration(now.Sub(controllerStarted))
		if uptime >= expected+expected/2 {
			// Advisory only for cron: a cron order that has never fired since
			// controller start may be the cron-scheduler bug (ga-97qngx), not
			// a real outage. Cooldown never-fired/stale paths remain blocking
			// because they indicate an execution gap.
			if order.Trigger == "cron" {
				return StatusError, SeverityAdvisory, fmt.Sprintf("%s: never fired since controller start %s ago", name, formatOrderFiringDuration(uptime))
			}
			return StatusError, SeverityBlocking, fmt.Sprintf("%s: never fired since controller start %s ago", name, formatOrderFiringDuration(uptime))
		}
		return StatusOK, SeverityBlocking, fmt.Sprintf("%s: never fired (controller running %s, within first cycle)", name, formatOrderFiringDuration(uptime))
	}

	age := nonNegativeDuration(now.Sub(lastFired))
	switch {
	case age >= expected*3:
		return StatusError, SeverityBlocking, fmt.Sprintf("%s: last fired %s ago, expected every %s (CRITICAL: stale)", name, formatOrderFiringDuration(age), formatOrderFiringDuration(expected))
	case age >= expected+expected/2:
		return StatusWarning, SeverityBlocking, fmt.Sprintf("%s: last fired %s ago, expected every %s (overdue)", name, formatOrderFiringDuration(age), formatOrderFiringDuration(expected))
	default:
		return StatusOK, SeverityBlocking, fmt.Sprintf("%s: last fired %s ago, expected every %s", name, formatOrderFiringDuration(age), formatOrderFiringDuration(expected))
	}
}

func orderDisplayName(order orders.Order) string {
	if order.Rig == "" {
		return order.Name
	}
	return order.ScopedName()
}

func orderHistoryHintTarget(order orders.Order) string {
	if order.Rig != "" {
		return fmt.Sprintf("%s --rig %s", order.Name, order.Rig)
	}
	return order.Name
}

func worseStatus(a, b CheckStatus) CheckStatus {
	if b > a {
		return b
	}
	return a
}

func nonNegativeDuration(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d
}

func formatOrderFiringDuration(d time.Duration) string {
	d = d.Round(time.Minute)
	if d == 0 {
		return "0s"
	}
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	if d%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return d.String()
}
