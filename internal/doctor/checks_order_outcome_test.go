package doctor

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
)

// outcomeEvent builds one order.completed / order.failed event. seq is what the
// counter orders by, so tests control sequence explicitly.
func outcomeEvent(seq uint64, subject, eventType string, ts time.Time, message string) events.Event {
	return events.Event{Seq: seq, Type: eventType, Ts: ts, Subject: subject, Message: message}
}

func TestConsecutiveOrderFailuresCountsTrailingFailures(t *testing.T) {
	base := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	outcomes := []events.Event{
		outcomeEvent(1, "refresh-family-clones:rig:st", events.OrderCompleted, base, ""),
		outcomeEvent(2, "refresh-family-clones:rig:st", events.OrderFailed, base.Add(6*time.Hour), "exit status 128"),
		outcomeEvent(3, "refresh-family-clones:rig:st", events.OrderFailed, base.Add(12*time.Hour), "exit status 128"),
		outcomeEvent(4, "refresh-family-clones:rig:st", events.OrderFailed, base.Add(18*time.Hour), "exit status 128"),
	}

	streak, lastMessage, sawOutcome, skipped := consecutiveOrderFailures(outcomes, "refresh-family-clones:rig:st", nil, 10*time.Minute)

	if streak != 3 {
		t.Fatalf("streak = %d, want 3", streak)
	}
	if lastMessage != "exit status 128" {
		t.Fatalf("lastMessage = %q, want %q", lastMessage, "exit status 128")
	}
	if !sawOutcome {
		t.Fatal("sawOutcome = false, want true")
	}
	if skipped != 0 {
		t.Fatalf("skipped = %d, want 0 — no starts provided", skipped)
	}
}

func TestConsecutiveOrderFailuresStopsAtSuccess(t *testing.T) {
	base := time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)
	// The dolt-remotes-patrol shape: many lifetime failures, currently healthy.
	outcomes := []events.Event{
		outcomeEvent(1, "dolt-remotes-patrol", events.OrderFailed, base, "exit status 1"),
		outcomeEvent(2, "dolt-remotes-patrol", events.OrderFailed, base.Add(15*time.Minute), "exit status 1"),
		outcomeEvent(3, "dolt-remotes-patrol", events.OrderFailed, base.Add(30*time.Minute), "exit status 1"),
		outcomeEvent(4, "dolt-remotes-patrol", events.OrderCompleted, base.Add(45*time.Minute), ""),
	}

	streak, _, sawOutcome, _ := consecutiveOrderFailures(outcomes, "dolt-remotes-patrol", nil, 10*time.Minute)

	if streak != 0 {
		t.Fatalf("streak = %d, want 0", streak)
	}
	if !sawOutcome {
		t.Fatal("sawOutcome = false, want true")
	}
}

func TestConsecutiveOrderFailuresIgnoresOtherOrders(t *testing.T) {
	base := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	outcomes := []events.Event{
		outcomeEvent(1, "beads-health", events.OrderFailed, base, "context canceled"),
		outcomeEvent(2, "dolt-health", events.OrderFailed, base.Add(time.Minute), "exit status 1"),
		outcomeEvent(3, "beads-health", events.OrderCompleted, base.Add(2*time.Minute), ""),
		outcomeEvent(4, "dolt-health", events.OrderFailed, base.Add(3*time.Minute), "exit status 1"),
	}

	streak, _, _, _ := consecutiveOrderFailures(outcomes, "dolt-health", nil, 10*time.Minute)

	if streak != 2 {
		t.Fatalf("streak = %d, want 2 (beads-health events must not interleave)", streak)
	}
}

func TestConsecutiveOrderFailuresReportsNoOutcomes(t *testing.T) {
	streak, lastMessage, sawOutcome, skipped := consecutiveOrderFailures(nil, "never-run", nil, 10*time.Minute)

	if streak != 0 || lastMessage != "" {
		t.Fatalf("streak/lastMessage = %d/%q, want 0/\"\"", streak, lastMessage)
	}
	if sawOutcome {
		t.Fatal("sawOutcome = true, want false for an order with no outcome events")
	}
	if skipped != 0 {
		t.Fatalf("skipped = %d, want 0", skipped)
	}
}

func TestConsecutiveOrderFailuresSkipsPostStartBurst(t *testing.T) {
	// gastownhall/gascity#3898: for ~5 min after supervisor start, exec orders
	// fail spuriously. A 30s-cooldown order logs ~10 consecutive such failures.
	start := time.Date(2026, 8, 4, 23, 23, 0, 0, time.UTC)
	outcomes := []events.Event{}
	for i := 0; i < 10; i++ {
		outcomes = append(outcomes, outcomeEvent(uint64(i+1), "dolt-health", events.OrderFailed,
			start.Add(time.Duration(i+1)*30*time.Second), "gc: unknown command \"dolt\""))
	}

	streak, _, sawOutcome, skipped := consecutiveOrderFailures(outcomes, "dolt-health", []time.Time{start}, 10*time.Minute)

	if streak != 0 {
		t.Fatalf("streak = %d, want 0 — every failure is inside the grace window", streak)
	}
	if !sawOutcome {
		t.Fatal("sawOutcome = false, want true")
	}
	if skipped != 10 {
		t.Fatalf("skipped = %d, want 10 — every failure was inside the grace window", skipped)
	}
}

func TestConsecutiveOrderFailuresSkipsWithoutResetting(t *testing.T) {
	// A skipped in-window failure must neither count nor break the streak:
	// resetting would let a frequently-restarting city zero a broken order forever.
	start := time.Date(2026, 8, 4, 23, 23, 0, 0, time.UTC)
	outcomes := []events.Event{
		outcomeEvent(1, "dolt-health", events.OrderCompleted, start.Add(-time.Hour), ""),
		outcomeEvent(2, "dolt-health", events.OrderFailed, start.Add(-30*time.Minute), "exit status 1"),
		outcomeEvent(3, "dolt-health", events.OrderFailed, start.Add(2*time.Minute), "context canceled"),
		outcomeEvent(4, "dolt-health", events.OrderFailed, start.Add(30*time.Minute), "exit status 1"),
	}

	streak, _, _, skipped := consecutiveOrderFailures(outcomes, "dolt-health", []time.Time{start}, 10*time.Minute)

	if streak != 2 {
		t.Fatalf("streak = %d, want 2 — the in-window failure is skipped, not counted, and must not reset", streak)
	}
	if skipped != 1 {
		t.Fatalf("skipped = %d, want 1", skipped)
	}
}

func TestConsecutiveOrderFailuresChecksEveryStart(t *testing.T) {
	// Two restarts with no successful run between them. Checking only the LATEST
	// start would count the older burst and manufacture a false positive.
	first := time.Date(2026, 8, 4, 20, 0, 0, 0, time.UTC)
	second := time.Date(2026, 8, 4, 23, 0, 0, 0, time.UTC)
	outcomes := []events.Event{
		outcomeEvent(1, "gate-sweep", events.OrderFailed, first.Add(time.Minute), "context canceled"),
		outcomeEvent(2, "gate-sweep", events.OrderFailed, first.Add(2*time.Minute), "context canceled"),
		outcomeEvent(3, "gate-sweep", events.OrderFailed, second.Add(time.Minute), "context canceled"),
		outcomeEvent(4, "gate-sweep", events.OrderFailed, second.Add(2*time.Minute), "context canceled"),
	}

	streak, _, _, skipped := consecutiveOrderFailures(outcomes, "gate-sweep", []time.Time{first, second}, 10*time.Minute)

	if streak != 0 {
		t.Fatalf("streak = %d, want 0 — all four failures sit inside one grace window or the other", streak)
	}
	if skipped != 4 {
		t.Fatalf("skipped = %d, want 4", skipped)
	}
}

func TestConsecutiveOrderFailuresUndercountsAcrossGraceWindow(t *testing.T) {
	// The dolt-remotes-patrol shape: a genuine 27-failure run with exactly one
	// failure landing near a controller start. "Skip" means neither count nor
	// break, so the answer is 26 — NOT 1 (which is what breaking would give) and
	// not 27. The undercount is accepted: counting in-window failures would
	// reintroduce the #3898 false positive, and a run long enough to span a
	// restart is already far past a threshold of 3.
	base := time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)
	start := base.Add(13 * time.Hour)
	outcomes := []events.Event{
		outcomeEvent(0, "dolt-remotes-patrol", events.OrderCompleted, base.Add(-time.Hour), ""),
	}
	seq := uint64(1)
	for i := 0; i < 27; i++ {
		ts := base.Add(time.Duration(i) * time.Hour)
		if i == 13 {
			ts = start.Add(2 * time.Minute) // the one inside the grace window
		}
		outcomes = append(outcomes, outcomeEvent(seq, "dolt-remotes-patrol", events.OrderFailed, ts, "exit status 1"))
		seq++
	}

	streak, _, _, skipped := consecutiveOrderFailures(outcomes, "dolt-remotes-patrol", []time.Time{start}, 10*time.Minute)

	if streak != 26 {
		t.Fatalf("streak = %d, want 26 (27 failures, one skipped inside the grace window, run not broken)", streak)
	}
	if skipped != 1 {
		t.Fatalf("skipped = %d, want 1", skipped)
	}
}

func TestConsecutiveOrderFailuresStopsAtSuccessInsideGraceWindow(t *testing.T) {
	// A success is proof the order works and ends the streak, even inside the
	// grace window. Skipping it would let the walk run past onto stale failures
	// from before the success — the exact false positive this check prevents.
	start := time.Date(2026, 8, 4, 23, 23, 0, 0, time.UTC)
	outcomes := []events.Event{
		outcomeEvent(1, "probe-order", events.OrderFailed, start.Add(-100*time.Hour), "exit status 1"),
		outcomeEvent(2, "probe-order", events.OrderFailed, start.Add(-99*time.Hour), "exit status 1"),
		outcomeEvent(3, "probe-order", events.OrderFailed, start.Add(-98*time.Hour), "exit status 1"),
		outcomeEvent(4, "probe-order", events.OrderCompleted, start.Add(2*time.Minute), ""),
	}

	streak, _, sawOutcome, skipped := consecutiveOrderFailures(outcomes, "probe-order", []time.Time{start}, 10*time.Minute)

	if streak != 0 {
		t.Fatalf("streak = %d, want 0 — the success inside the grace window must end the walk", streak)
	}
	if !sawOutcome {
		t.Fatal("sawOutcome = false, want true")
	}
	if skipped != 0 {
		t.Fatalf("skipped = %d, want 0 — the success ends the walk before any failure is inspected", skipped)
	}
}

func TestConsecutiveOrderFailuresPrefersNewestMessageEvenWhenEmpty(t *testing.T) {
	base := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	outcomes := []events.Event{
		outcomeEvent(1, "probe-order", events.OrderFailed, base, "real reason"),
		outcomeEvent(2, "probe-order", events.OrderFailed, base.Add(time.Hour), ""),
	}

	streak, lastMessage, _, _ := consecutiveOrderFailures(outcomes, "probe-order", nil, 10*time.Minute)

	if streak != 2 {
		t.Fatalf("streak = %d, want 2", streak)
	}
	if lastMessage != "" {
		t.Fatalf("lastMessage = %q, want \"\" — the newest failure's message wins even when empty", lastMessage)
	}
}

func TestNearControllerStartBoundaries(t *testing.T) {
	start := time.Date(2026, 8, 4, 23, 0, 0, 0, time.UTC)
	grace := 10 * time.Minute

	cases := []struct {
		name string
		ts   time.Time
		want bool
	}{
		{"before start", start.Add(-time.Second), false},
		{"at start", start, true},
		{"inside window", start.Add(5 * time.Minute), true},
		{"at boundary", start.Add(grace), true},
		{"past boundary", start.Add(grace + time.Second), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nearControllerStart(tc.ts, []time.Time{start}, grace); got != tc.want {
				t.Fatalf("nearControllerStart(%v) = %v, want %v", tc.ts, got, tc.want)
			}
		})
	}
}

func TestNearControllerStartWithNoStarts(t *testing.T) {
	ts := time.Date(2026, 8, 4, 23, 0, 0, 0, time.UTC)
	if nearControllerStart(ts, nil, 10*time.Minute) {
		t.Fatal("nearControllerStart = true with no starts, want false")
	}
}

func TestClassifyOrderOutcomeFlagsAtThreshold(t *testing.T) {
	order := orders.Order{Name: "refresh-family-clones", Rig: "st"}

	status, detail := classifyOrderOutcome(order, 3, 3, "exit status 128", true, 0)

	if status != StatusWarning {
		t.Fatalf("status = %v, want StatusWarning", status)
	}
	if !strings.Contains(detail, "3 consecutive failures") {
		t.Fatalf("detail = %q, want it to state the streak", detail)
	}
	if !strings.Contains(detail, "exit status 128") {
		t.Fatalf("detail = %q, want it to carry the last failure message", detail)
	}
}

func TestClassifyOrderOutcomeAllowsUnderThreshold(t *testing.T) {
	order := orders.Order{Name: "dolt-remotes-patrol"}

	status, detail := classifyOrderOutcome(order, 2, 3, "exit status 1", true, 0)

	if status != StatusOK {
		t.Fatalf("status = %v, want StatusOK for a streak under threshold", status)
	}
	if !strings.Contains(detail, "2 consecutive") {
		t.Fatalf("detail = %q, want it to report the sub-threshold streak", detail)
	}
}

func TestClassifyOrderOutcomeHealthyOrder(t *testing.T) {
	order := orders.Order{Name: "gate-sweep"}

	status, detail := classifyOrderOutcome(order, 0, 3, "", true, 0)

	if status != StatusOK {
		t.Fatalf("status = %v, want StatusOK", status)
	}
	if !strings.Contains(detail, "last run succeeded") {
		t.Fatalf("detail = %q, want it to say the last run succeeded", detail)
	}
}

func TestClassifyOrderOutcomeNoOutcomesYet(t *testing.T) {
	order := orders.Order{Name: "brand-new-order"}

	status, detail := classifyOrderOutcome(order, 0, 3, "", false, 0)

	if status != StatusOK {
		t.Fatalf("status = %v, want StatusOK — order-firing-current owns the never-fired case", status)
	}
	if !strings.Contains(detail, "no completed runs yet") {
		t.Fatalf("detail = %q, want it to distinguish never-ran from succeeded", detail)
	}
}

func TestClassifyOrderOutcomeOmitsEmptyMessage(t *testing.T) {
	order := orders.Order{Name: "some-order"}

	_, detail := classifyOrderOutcome(order, 3, 3, "", true, 0)

	if strings.Contains(detail, `""`) {
		t.Fatalf("detail = %q, want no empty-quote artifact when the message is blank", detail)
	}
}

func TestClassifyOrderOutcomeReportsGraceWindowSkipsInsteadOfSuccess(t *testing.T) {
	// gastownhall/gascity FIX 4: when every trailing failure sits inside the
	// controller-start grace window, streak == 0 but the order did NOT
	// succeed. Claiming "last run succeeded" would be a false statement in
	// the diagnostic tool at exactly the moment an operator reads it — just
	// after a restart.
	order := orders.Order{Name: "dolt-health"}

	status, detail := classifyOrderOutcome(order, 0, 3, "", true, 10)

	if status != StatusOK {
		t.Fatalf("status = %v, want StatusOK — this suppresses the alarm as intended", status)
	}
	if strings.Contains(detail, "last run succeeded") {
		t.Fatalf("detail = %q, must not claim success when every trailing run actually failed", detail)
	}
	if !strings.Contains(detail, "10 recent failure(s) within controller-start grace window") {
		t.Fatalf("detail = %q, want it to report the grace-window-skipped count instead", detail)
	}
}

func TestOrderOutcomeHealthySkipsManualOrders(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, "orders"), 0o755); err != nil {
		t.Fatal(err)
	}
	// One scheduled, one manual. Only the scheduled one may appear in details:
	// ticket-intake sat at a permanent streak of 20 from 2026-06-17 purely
	// because it was switched to trigger="manual".
	if err := os.WriteFile(filepath.Join(cityDir, "orders", "scheduled-order.toml"),
		[]byte("[order]\nexec = \"true\"\ntrigger = \"cooldown\"\ninterval = \"5m\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "orders", "manual-order.toml"),
		[]byte("[order]\nexec = \"true\"\ntrigger = \"manual\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{Workspace: config.Workspace{Name: "demo"}}
	result := NewOrderOutcomeHealthyCheck(cfg, cityDir).Run(&CheckContext{CityPath: cityDir})

	if result.Severity != SeverityAdvisory {
		t.Fatalf("severity = %v, want SeverityAdvisory", result.Severity)
	}
	joined := strings.Join(result.Details, "\n")
	if !strings.Contains(joined, "scheduled-order") {
		t.Fatalf("scheduled order missing from details:\n%s", joined)
	}
	if strings.Contains(joined, "manual-order") {
		t.Fatalf("manual order must be out of scope by construction:\n%s", joined)
	}
}

func TestOrderOutcomeHealthyReportsCleanCity(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, "orders"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "orders", "scheduled-order.toml"),
		[]byte("[order]\nexec = \"true\"\ntrigger = \"cooldown\"\ninterval = \"5m\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{Workspace: config.Workspace{Name: "demo"}}
	// No events.jsonl at all: a missing log must read as "nothing to report",
	// not as an error, or a fresh city fails doctor on its first run.
	result := NewOrderOutcomeHealthyCheck(cfg, cityDir).Run(&CheckContext{CityPath: cityDir})

	if result.Status != StatusOK {
		t.Fatalf("status = %v (%s), want StatusOK on a city with no event log", result.Status, result.Message)
	}
}

// TestOrderOutcomeHealthy_FlagsRigScopedOrderFailureStreak is an end-to-end
// Run test using a RIG-SCOPED order and a real .gc/events.jsonl. FIX 2:
// neither pre-existing Run test used a rig-scoped order or wrote outcome
// events, so nothing exercised the event read, the Seq merge, the
// order.ScopedName() subject match, the failing-order message, or FixHint.
// Swapping order.ScopedName() for order.Name on the subject-match line blinds
// the check to every rig-scoped order — including the one whose 3-day silent
// failure is the motivating case for this check — while passing every other
// test in the suite. This test is written to fail under that regression.
func TestOrderOutcomeHealthy_FlagsRigScopedOrderFailureStreak(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)

	rigPath := filepath.Join(cityPath, "rigs", "st")
	rigFormulas := filepath.Join(rigPath, "formulas")
	rigOrders := filepath.Join(rigPath, "orders")
	if err := os.MkdirAll(rigOrders, 0o755); err != nil {
		t.Fatalf("creating rig orders dir: %v", err)
	}
	cfg.Rigs = []config.Rig{{Name: "st", Path: rigPath}}
	cfg.FormulaLayers.Rigs = map[string][]string{"st": {cfg.FormulaLayers.City[0], rigFormulas}}
	writeOrderFiringTestOrderInDir(t, rigOrders, "refresh-family-clones", "cooldown", "6h")

	// order.ScopedName() for Rig "st" is "refresh-family-clones:rig:st" —
	// see orders.Order.ScopedName(). The recorded events must use that exact
	// scoped subject, matching how the real dispatcher records order outcomes.
	scopedSubject := "refresh-family-clones:rig:st"
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.OrderFailed, Ts: now.Add(-18 * time.Hour), Subject: scopedSubject, Message: "exit status 128"},
		events.Event{Type: events.OrderFailed, Ts: now.Add(-12 * time.Hour), Subject: scopedSubject, Message: "exit status 128"},
		events.Event{Type: events.OrderFailed, Ts: now.Add(-6 * time.Hour), Subject: scopedSubject, Message: "exit status 128"},
	)

	result := NewOrderOutcomeHealthyCheck(cfg, cityPath).Run(&CheckContext{CityPath: cityPath})

	if result.Status != StatusWarning {
		t.Fatalf("status = %v, want StatusWarning; msg=%s details=%v", result.Status, result.Message, result.Details)
	}
	if result.Severity != SeverityAdvisory {
		t.Fatalf("severity = %v, want SeverityAdvisory", result.Severity)
	}
	if result.Message != "1 order(s) failing repeatedly" {
		t.Fatalf("message = %q, want it to name exactly 1 failing order", result.Message)
	}
	joined := strings.Join(result.Details, "\n")
	if !strings.Contains(joined, "3 consecutive failures") {
		t.Fatalf("details = %v, want them to carry the streak count", result.Details)
	}
	wantHint := "Inspect with: gc order check && gc order history refresh-family-clones --rig st"
	if result.FixHint != wantHint {
		t.Fatalf("FixHint = %q, want %q — gc order history takes a bare name positionally", result.FixHint, wantHint)
	}
}

// TestOrderFiringCurrentAndOrderOutcomeHealthy_MonitorSameOrderSet pins the
// keystone property the design relies on: order-firing-current and
// order-outcome-healthy must monitor exactly the same order set (both share
// scanOrderFiringCurrentOrders), which is what excludes manual orders without
// a recency heuristic. Without this test, a future change to one filter chain
// could silently diverge the pair with every other test still green.
func TestOrderFiringCurrentAndOrderOutcomeHealthy_MonitorSameOrderSet(t *testing.T) {
	cityPath, cfg := orderFiringTestCity(t)
	ordersDir := filepath.Join(cityPath, "orders")
	writeOrderFiringTestOrderInDir(t, ordersDir, "cooldown-order", "cooldown", "5m")
	writeOrderFiringTestOrderInDir(t, ordersDir, "cron-order", "cron", "*/5 * * * *")
	writeOrderFiringTestOrderInDir(t, ordersDir, "manual-order", "manual", "")
	if err := os.WriteFile(filepath.Join(ordersDir, "disabled-order.toml"),
		[]byte("[order]\nexec = \"true\"\ntrigger = \"cooldown\"\ninterval = \"5m\"\nenabled = false\n"), 0o644); err != nil {
		t.Fatalf("write disabled-order.toml: %v", err)
	}

	firingResult := NewOrderFiringCurrentCheck(cfg, cityPath).Run(&CheckContext{CityPath: cityPath})
	outcomeResult := NewOrderOutcomeHealthyCheck(cfg, cityPath).Run(&CheckContext{CityPath: cityPath})

	firingNames := orderOutcomeTestDetailNames(firingResult.Details)
	outcomeNames := orderOutcomeTestDetailNames(outcomeResult.Details)

	if len(firingNames) == 0 {
		t.Fatalf("order-firing-current monitored no orders; details = %v", firingResult.Details)
	}
	if got, want := strings.Join(outcomeNames, ","), strings.Join(firingNames, ","); got != want {
		t.Fatalf("monitored order sets diverge:\n  order-firing-current:   %v\n  order-outcome-healthy:  %v", firingNames, outcomeNames)
	}
	for _, excluded := range []string{"manual-order", "disabled-order"} {
		for _, name := range append(append([]string{}, firingNames...), outcomeNames...) {
			if name == excluded {
				t.Fatalf("%s must be out of scope for both checks, got names %v / %v", excluded, firingNames, outcomeNames)
			}
		}
	}
}

// orderOutcomeTestDetailNames extracts the order display name from each
// detail line. Both checks' details begin with the order display name
// followed by ": ".
func orderOutcomeTestDetailNames(details []string) []string {
	names := make([]string, 0, len(details))
	for _, d := range details {
		name, _, ok := strings.Cut(d, ": ")
		if !ok {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
