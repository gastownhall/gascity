package doctor

import (
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/orders"
)

const (
	orderFiringCurrentName    = "order-firing-current"
	orderFiringInspectHintFmt = "Inspect with: gc order check && gc order history %s"
)

// OrderFiringCurrentCheck reports scheduled orders whose last firing is stale.
type OrderFiringCurrentCheck struct {
	cfg      *config.City
	cityPath string
	clock    func() time.Time
}

// NewOrderFiringCurrentCheck creates a check for cron and cooldown order freshness.
func NewOrderFiringCurrentCheck(cfg *config.City, cityPath string) *OrderFiringCurrentCheck {
	return &OrderFiringCurrentCheck{
		cfg:      cfg,
		cityPath: cityPath,
		clock:    time.Now,
	}
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

	for _, order := range allOrders {
		if order.Trigger != "cron" && order.Trigger != "cooldown" {
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
			continue
		}
		status, detail := classifyOrderFiring(order, now, expected, latestOrderFiredAt(firedEvents, order.ScopedName()), startedAt)
		worst = worseStatus(worst, status)
		result.Details = append(result.Details, detail)
		if status != StatusOK && firstNonOK == "" {
			firstNonOK = orderHistoryHintTarget(order)
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
	if firstNonOK != "" {
		result.FixHint = fmt.Sprintf(orderFiringInspectHintFmt, firstNonOK)
	}
	return result
}

func scanOrderFiringCurrentOrders(cityPath string, cfg *config.City) ([]orders.Order, error) {
	cityLayers := orderFiringCityFormulaLayers(cityPath, cfg)
	cityOrders, err := orders.ScanRoots(fsys.OSFS{}, orderFiringCityOrderRoots(cityPath, cfg), cfg.Orders.Skip)
	if err != nil {
		return nil, err
	}

	var rigOrders []orders.Order
	rigNames := make([]string, 0, len(cfg.FormulaLayers.Rigs))
	for rigName := range cfg.FormulaLayers.Rigs {
		rigNames = append(rigNames, rigName)
	}
	sort.Strings(rigNames)
	for _, rigName := range rigNames {
		exclusive := orderFiringRigExclusiveLayers(cfg.FormulaLayers.Rigs[rigName], cityLayers)
		if len(exclusive) == 0 {
			continue
		}
		aa, err := orders.ScanRoots(fsys.OSFS{}, orderFiringRigOrderRoots(exclusive), cfg.Orders.Skip)
		if err != nil {
			return nil, fmt.Errorf("rig %s: %w", rigName, err)
		}
		for i := range aa {
			aa[i].Rig = rigName
		}
		rigOrders = append(rigOrders, aa...)
	}

	allOrders := make([]orders.Order, 0, len(cityOrders)+len(rigOrders))
	allOrders = append(allOrders, cityOrders...)
	allOrders = append(allOrders, rigOrders...)
	if len(cfg.Orders.Overrides) > 0 {
		if err := orders.ApplyOverrides(allOrders, orderFiringOverrides(cfg.Orders.Overrides)); err != nil {
			return nil, err
		}
	}
	return orders.FilterEnabled(allOrders), nil
}

func orderFiringCityFormulaLayers(cityPath string, cfg *config.City) []string {
	if len(cfg.FormulaLayers.City) > 0 {
		return cfg.FormulaLayers.City
	}
	return []string{citylayout.ResolveFormulasDir(cityPath, cfg.FormulasDir())}
}

func orderFiringCityOrderRoots(cityPath string, cfg *config.City) []orders.ScanRoot {
	formulaLayers := orderFiringCityFormulaLayers(cityPath, cfg)
	localFormulas := citylayout.ResolveFormulasDir(cityPath, cfg.FormulasDir())
	roots := make([]orders.ScanRoot, 0, len(formulaLayers))
	seen := make(map[string]bool, len(formulaLayers))
	for _, layer := range formulaLayers {
		root := orders.ScanRoot{
			Dir:          filepath.Join(filepath.Dir(layer), "orders"),
			FormulaLayer: layer,
		}
		if layer == localFormulas {
			root.Dir = citylayout.OrdersPath(cityPath)
		}
		key := filepath.Clean(root.Dir) + "\n" + filepath.Clean(root.FormulaLayer)
		if seen[key] {
			continue
		}
		seen[key] = true
		roots = append(roots, root)
	}
	return roots
}

func orderFiringRigOrderRoots(formulaLayers []string) []orders.ScanRoot {
	roots := make([]orders.ScanRoot, 0, len(formulaLayers))
	for _, layer := range formulaLayers {
		roots = append(roots, orders.ScanRoot{
			Dir:          filepath.Join(filepath.Dir(layer), "orders"),
			FormulaLayer: layer,
		})
	}
	return roots
}

func orderFiringRigExclusiveLayers(rigLayers, cityLayers []string) []string {
	if len(rigLayers) <= len(cityLayers) {
		return nil
	}
	return rigLayers[len(cityLayers):]
}

func orderFiringOverrides(cfgOverrides []config.OrderOverride) []orders.Override {
	out := make([]orders.Override, len(cfgOverrides))
	for i, override := range cfgOverrides {
		out[i] = orders.Override{
			Name:     override.Name,
			Rig:      override.Rig,
			Enabled:  override.Enabled,
			Trigger:  override.Trigger,
			Interval: override.Interval,
			Schedule: override.Schedule,
			Check:    override.Check,
			On:       override.On,
			Pool:     override.Pool,
			Timeout:  override.Timeout,
		}
	}
	return out
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

	const day = 24 * time.Hour
	base := time.Date(2026, 5, 12, 0, 0, 0, 0, time.UTC)
	matches := make([]time.Time, 0, 1440)
	for i := 0; i < 1440; i++ {
		ts := base.Add(time.Duration(i) * time.Minute)
		matched, err := cronScheduleMatchesAt(fields, ts)
		if err != nil {
			return 0, err
		}
		if matched {
			matches = append(matches, ts)
		}
	}
	switch len(matches) {
	case 0:
		return 0, fmt.Errorf("cron schedule %q has no firing minutes in a 24h window", schedule)
	case 1:
		return day, nil
	}

	minGap := day
	for i := 1; i < len(matches); i++ {
		gap := matches[i].Sub(matches[i-1])
		if gap < minGap {
			minGap = gap
		}
	}
	wrapGap := matches[0].Add(day).Sub(matches[len(matches)-1])
	if wrapGap < minGap {
		minGap = wrapGap
	}
	return minGap, nil
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

func classifyOrderFiring(order orders.Order, now time.Time, expected time.Duration, lastFired, controllerStarted time.Time) (CheckStatus, string) {
	name := orderDisplayName(order)
	if lastFired.IsZero() {
		if controllerStarted.IsZero() {
			return StatusOK, fmt.Sprintf("%s: never fired (controller start unknown)", name)
		}
		uptime := nonNegativeDuration(now.Sub(controllerStarted))
		if uptime >= expected+expected/2 {
			return StatusError, fmt.Sprintf("%s: never fired since controller start %s ago", name, formatOrderFiringDuration(uptime))
		}
		return StatusOK, fmt.Sprintf("%s: never fired (controller running %s, within first cycle)", name, formatOrderFiringDuration(uptime))
	}

	age := nonNegativeDuration(now.Sub(lastFired))
	switch {
	case age >= expected*3:
		return StatusError, fmt.Sprintf("%s: last fired %s ago, expected every %s (CRITICAL: stale)", name, formatOrderFiringDuration(age), formatOrderFiringDuration(expected))
	case age >= expected+expected/2:
		return StatusWarning, fmt.Sprintf("%s: last fired %s ago, expected every %s (overdue)", name, formatOrderFiringDuration(age), formatOrderFiringDuration(expected))
	default:
		return StatusOK, fmt.Sprintf("%s: last fired %s ago, expected every %s", name, formatOrderFiringDuration(age), formatOrderFiringDuration(expected))
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
