package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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

// skippedOrderEvent builds the typed order.skipped declaration the controller
// records after a successful exec run that declared incomplete work.
func skippedOrderEvent(subject string, ts time.Time, outcome, reason string, scopes ...events.OrderSkippedScope) events.Event {
	message := fmt.Sprintf("%s: %s", outcome, reason)
	names := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		names = append(names, scope.Scope+" ("+scope.Reason+")")
	}
	if len(names) > 0 {
		message += "; not run: " + strings.Join(names, ", ")
	}
	if scopes == nil {
		scopes = []events.OrderSkippedScope{}
	}
	return events.Event{
		Type: events.OrderSkipped, Ts: ts, Subject: subject, Actor: "controller", Message: message,
		Payload: events.OrderSkippedPayloadJSON(events.OrderSkippedPayload{
			OrderName: subject, Outcome: outcome, Reason: reason, Scopes: scopes,
		}),
	}
}

// TestOrderOutcomeHealthyRecordedIncompleteOutcomes pins that a monitored
// order whose run recorded order.completed followed by an order.skipped
// declaration surfaces the declaration as an advisory warning immediately,
// instead of reporting the companion completion as success.
func TestOrderOutcomeHealthyRecordedIncompleteOutcomes(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	partial := skippedOrderEvent("export-sweep", now.Add(-time.Hour), "partial", "store unreachable",
		events.OrderSkippedScope{Scope: "rig:st", Reason: "dolt not reachable"})
	skipped := skippedOrderEvent("nightly-digest", now.Add(-time.Hour), "skipped", "nothing exported",
		events.OrderSkippedScope{Scope: "rig:a", Reason: "suspended"}, events.OrderSkippedScope{Scope: "rig:b", Reason: "no remote"})
	unreadable := events.Event{
		Type: events.OrderSkipped, Ts: now.Add(-time.Hour), Subject: "broken-declarer", Actor: "controller",
		Message: "unreadable outcome declaration: invalid character 'x' looking for beginning of value",
	}
	blank := events.Event{Type: events.OrderSkipped, Ts: now.Add(-time.Hour), Subject: "blank-declarer", Actor: "controller"}

	cases := []struct {
		name       string
		trigger    string
		timing     string
		order      string
		evts       []events.Event
		wantDetail string
	}{
		{
			name: "cooldown partial after completion", trigger: "cooldown", timing: "6h", order: "export-sweep",
			evts: []events.Event{
				{Type: events.OrderCompleted, Ts: now.Add(-time.Hour), Subject: "export-sweep", Actor: "controller"},
				partial,
			},
			wantDetail: "export-sweep: recorded incomplete work: partial: store unreachable; not run: rig:st (dolt not reachable)",
		},
		{
			name: "cron skipped after completion", trigger: "cron", timing: "0 3 * * *", order: "nightly-digest",
			evts: []events.Event{
				{Type: events.OrderCompleted, Ts: now.Add(-time.Hour), Subject: "nightly-digest", Actor: "controller"},
				skipped,
			},
			wantDetail: "nightly-digest: recorded incomplete work: skipped: nothing exported; not run: rig:a (suspended), rig:b (no remote)",
		},
		{
			name: "declaration without companion completion", trigger: "cooldown", timing: "6h", order: "export-sweep",
			evts:       []events.Event{partial},
			wantDetail: "export-sweep: recorded incomplete work: partial: store unreachable",
		},
		{
			name: "unreadable declaration keeps its error text", trigger: "cooldown", timing: "1h", order: "broken-declarer",
			evts: []events.Event{
				{Type: events.OrderCompleted, Ts: now.Add(-time.Hour), Subject: "broken-declarer", Actor: "controller"},
				unreadable,
			},
			wantDetail: "broken-declarer: recorded incomplete work: unreadable outcome declaration: invalid character 'x'",
		},
		{
			name: "blank declaration message still warns", trigger: "cooldown", timing: "1h", order: "blank-declarer",
			evts: []events.Event{
				{Type: events.OrderCompleted, Ts: now.Add(-time.Hour), Subject: "blank-declarer", Actor: "controller"},
				blank,
			},
			wantDetail: "blank-declarer: recorded incomplete work",
		},
		{
			name: "declaration inside controller-start grace still warns", trigger: "cooldown", timing: "6h", order: "export-sweep",
			evts: []events.Event{
				{Type: events.ControllerStarted, Ts: now.Add(-time.Hour - time.Minute), Subject: "gc", Actor: "controller"},
				{Type: events.OrderCompleted, Ts: now.Add(-time.Hour), Subject: "export-sweep", Actor: "controller"},
				partial,
			},
			wantDetail: "export-sweep: recorded incomplete work: partial: store unreachable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cityPath, cfg := orderFiringTestCity(t)
			writeOrderFiringTestOrder(t, cityPath, tc.order, tc.trigger, tc.timing)
			writeOrderFiringTestEvents(t, cityPath, tc.evts...)

			result := NewOrderOutcomeHealthyCheck(cfg, cityPath).Run(&CheckContext{CityPath: cityPath})

			if result.Status != StatusWarning {
				t.Fatalf("status = %v, want StatusWarning; msg=%q details=%v", result.Status, result.Message, result.Details)
			}
			if result.Severity != SeverityAdvisory {
				t.Fatalf("severity = %v, want SeverityAdvisory", result.Severity)
			}
			joined := strings.Join(result.Details, "\n")
			if !strings.Contains(joined, tc.wantDetail) {
				t.Fatalf("details = %v, want a detail containing %q", result.Details, tc.wantDetail)
			}
			if strings.Contains(joined, "last run succeeded") || strings.Contains(joined, "no completed runs yet") {
				t.Fatalf("details = %v, must not claim success for an order with outstanding incomplete work", result.Details)
			}
			if result.Message == "all scheduled orders succeeding" {
				t.Fatalf("message = %q, must not claim every order succeeded", result.Message)
			}
			if want := fmt.Sprintf(orderOutcomeInspectHintFmt, tc.order); result.FixHint != want {
				t.Fatalf("FixHint = %q, want %q", result.FixHint, want)
			}
		})
	}
}

// TestLatestIncompleteOrderOutcome pins the declaration walk: newest-first by
// Seq, a completion clears every older declaration, failures neither clear nor
// hide one, and executions are never counted or paired.
func TestLatestIncompleteOrderOutcome(t *testing.T) {
	base := time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC)
	const subject = "export-sweep:rig:st"
	c := func(seq uint64) events.Event { return outcomeEvent(seq, subject, events.OrderCompleted, base, "") }
	f := func(seq uint64) events.Event {
		return outcomeEvent(seq, subject, events.OrderFailed, base, "exit status 1")
	}
	s := func(seq uint64, msg string) events.Event {
		return outcomeEvent(seq, subject, events.OrderSkipped, base, msg)
	}

	cases := []struct {
		name     string
		outcomes []events.Event
		wantOK   bool
		wantMsg  string
	}{
		{"empty", nil, false, ""},
		{"completion only", []events.Event{c(1), c(2)}, false, ""},
		{"[C,S] warns", []events.Event{c(1), s(2, "partial: one")}, true, "partial: one"},
		{"[C,S,C] clears", []events.Event{c(1), s(2, "partial: one"), c(3)}, false, ""},
		{"[C,S,C,S] keeps newest only", []events.Event{c(1), s(2, "partial: one"), c(3), s(4, "skipped: two")}, true, "skipped: two"},
		{"[C1,C2,S1,S2] quotes newest without counting", []events.Event{c(1), c(2), s(3, "partial: one"), s(4, "partial: two")}, true, "partial: two"},
		{"[C1,S1,C2] clears without attribution", []events.Event{c(1), s(2, "partial: one"), c(3)}, false, ""},
		{"[C,S,F] failure does not clear", []events.Event{c(1), s(2, "partial: one"), f(3)}, true, "partial: one"},
		{"[C,S,F,F,F] failures do not clear", []events.Event{c(1), s(2, "partial: one"), f(3), f(4), f(5)}, true, "partial: one"},
		{"[S] without companion", []events.Event{s(1, "partial: one")}, true, "partial: one"},
		{"blank newest does not select older message", []events.Event{c(1), s(2, "partial: one"), c(3), s(4, "")}, true, ""},
		{
			"decreasing Ts with increasing Seq clears",
			[]events.Event{
				outcomeEvent(1, subject, events.OrderSkipped, base.Add(time.Hour), "partial: one"),
				outcomeEvent(2, subject, events.OrderCompleted, base, ""),
			},
			false, "",
		},
		{
			"equal Ts with increasing Seq warns",
			[]events.Event{
				outcomeEvent(1, subject, events.OrderCompleted, base, ""),
				outcomeEvent(2, subject, events.OrderSkipped, base, "partial: one"),
			},
			true, "partial: one",
		},
		{
			"other subject completion does not clear",
			[]events.Event{s(1, "partial: one"), outcomeEvent(2, "export-sweep", events.OrderCompleted, base, "")},
			true, "partial: one",
		},
		{
			"other subject declaration is ignored",
			[]events.Event{c(1), outcomeEvent(2, "export-sweep", events.OrderSkipped, base, "partial: city")},
			false, "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := latestIncompleteOrderOutcome(tc.outcomes, subject)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (got %+v)", ok, tc.wantOK, got)
			}
			if ok && got.Message != tc.wantMsg {
				t.Fatalf("message = %q, want %q", got.Message, tc.wantMsg)
			}
		})
	}
}

// TestOrderOutcomeHealthyCompletionClearsIncompleteOutcome pins that a
// higher-Seq order.completed clears a recorded declaration, even when its
// timestamp is not later, and the existing success detail returns.
func TestOrderOutcomeHealthyCompletionClearsIncompleteOutcome(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		ts   []time.Time
	}{
		{"later timestamps", []time.Time{now.Add(-3 * time.Hour), now.Add(-2 * time.Hour), now.Add(-time.Hour)}},
		{"equal timestamps", []time.Time{now, now, now}},
		{"decreasing timestamps", []time.Time{now, now.Add(-time.Hour), now.Add(-2 * time.Hour)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cityPath, cfg := orderFiringTestCity(t)
			writeOrderFiringTestOrder(t, cityPath, "export-sweep", "cooldown", "6h")
			writeOrderFiringTestEvents(t, cityPath,
				events.Event{Type: events.OrderCompleted, Ts: tc.ts[0], Subject: "export-sweep", Actor: "controller"},
				skippedOrderEvent("export-sweep", tc.ts[1], "partial", "store unreachable",
					events.OrderSkippedScope{Scope: "rig:st", Reason: "dolt not reachable"}),
				events.Event{Type: events.OrderCompleted, Ts: tc.ts[2], Subject: "export-sweep", Actor: "controller"},
			)

			result := NewOrderOutcomeHealthyCheck(cfg, cityPath).Run(&CheckContext{CityPath: cityPath})

			if result.Status != StatusOK {
				t.Fatalf("status = %v, want StatusOK once a later completion is recorded; msg=%q details=%v", result.Status, result.Message, result.Details)
			}
			if result.Message != "all scheduled orders succeeding" {
				t.Fatalf("message = %q, want the existing success aggregate", result.Message)
			}
			if len(result.Details) != 1 || result.Details[0] != "export-sweep: last run succeeded" {
				t.Fatalf("details = %v, want exactly the existing success detail", result.Details)
			}
			if result.FixHint != "" {
				t.Fatalf("FixHint = %q, want none for a healthy order", result.FixHint)
			}
		})
	}
}

// TestConsecutiveOrderFailuresIgnoresDeclarations pins that order.skipped
// declarations are invisible to the hard-failure calculation: all four outputs
// equal those for the same stream with declarations removed.
func TestConsecutiveOrderFailuresIgnoresDeclarations(t *testing.T) {
	start := time.Date(2026, 8, 4, 23, 0, 0, 0, time.UTC)
	const subject = "export-sweep"
	at := func(seq uint64, eventType string, ts time.Time) events.Event {
		message := ""
		switch eventType {
		case events.OrderFailed:
			message = fmt.Sprintf("exit status %d", seq)
		case events.OrderSkipped:
			message = "partial: store unreachable"
		}
		return outcomeEvent(seq, subject, eventType, ts, message)
	}
	old := start.Add(-24 * time.Hour)
	inGrace := start.Add(time.Minute)
	cases := []struct {
		name     string
		outcomes []events.Event
	}{
		{"[C,F,F,F,S] keeps the streak", []events.Event{
			at(1, events.OrderCompleted, old), at(2, events.OrderFailed, old), at(3, events.OrderFailed, old),
			at(4, events.OrderFailed, old), at(5, events.OrderSkipped, old),
		}},
		{"[C,S,F,F,F] counts failures past the declaration", []events.Event{
			at(1, events.OrderCompleted, old), at(2, events.OrderSkipped, old), at(3, events.OrderFailed, old),
			at(4, events.OrderFailed, old), at(5, events.OrderFailed, old),
		}},
		{"[F,S,F] declaration does not reset", []events.Event{
			at(1, events.OrderFailed, old), at(2, events.OrderSkipped, old), at(3, events.OrderFailed, old),
		}},
		{"declaration only is not an outcome", []events.Event{at(1, events.OrderSkipped, old)}},
		{"other subjects are ignored", []events.Event{
			at(1, events.OrderFailed, old),
			outcomeEvent(2, "other-order", events.OrderSkipped, old, "partial: x"),
			outcomeEvent(3, "other-order", events.OrderCompleted, old, ""),
			at(4, events.OrderSkipped, old),
		}},
		{"completion inside grace still ends the walk", []events.Event{
			at(1, events.OrderFailed, old), at(2, events.OrderCompleted, inGrace), at(3, events.OrderSkipped, inGrace),
		}},
		{"grace-skipped failures around a declaration", []events.Event{
			at(1, events.OrderCompleted, old), at(2, events.OrderFailed, inGrace), at(3, events.OrderSkipped, inGrace),
			at(4, events.OrderFailed, inGrace),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withoutDeclarations := make([]events.Event, 0, len(tc.outcomes))
			for _, event := range tc.outcomes {
				if event.Type != events.OrderSkipped {
					withoutDeclarations = append(withoutDeclarations, event)
				}
			}
			starts := []time.Time{start}
			gotStreak, gotMessage, gotSaw, gotSkipped := consecutiveOrderFailures(tc.outcomes, subject, starts, 10*time.Minute)
			wantStreak, wantMessage, wantSaw, wantSkipped := consecutiveOrderFailures(withoutDeclarations, subject, starts, 10*time.Minute)
			if gotStreak != wantStreak || gotMessage != wantMessage || gotSaw != wantSaw || gotSkipped != wantSkipped {
				t.Fatalf("with declarations = (%d, %q, %v, %d), want (%d, %q, %v, %d) as without them",
					gotStreak, gotMessage, gotSaw, gotSkipped, wantStreak, wantMessage, wantSaw, wantSkipped)
			}
		})
	}
}

// TestOrderOutcomeHealthyIncompleteOutcomeWithFailures pins that a recorded
// declaration and trailing failures are reported side by side: the failure
// detail is kept exactly and the newest declaration is added to it.
func TestOrderOutcomeHealthyIncompleteOutcomeWithFailures(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	const declDetail = "export-sweep: recorded incomplete work: partial: store unreachable; not run: rig:st (dolt not reachable)"
	completed := func(ts time.Time) events.Event {
		return events.Event{Type: events.OrderCompleted, Ts: ts, Subject: "export-sweep", Actor: "controller"}
	}
	failed := func(ts time.Time) events.Event {
		return events.Event{Type: events.OrderFailed, Ts: ts, Subject: "export-sweep", Actor: "controller", Message: "exit status 1"}
	}
	declared := func(ts time.Time) events.Event {
		return skippedOrderEvent("export-sweep", ts, "partial", "store unreachable",
			events.OrderSkippedScope{Scope: "rig:st", Reason: "dolt not reachable"})
	}
	older := func(ts time.Time) events.Event {
		return skippedOrderEvent("export-sweep", ts, "skipped", "older declaration")
	}
	started := func(ts time.Time) events.Event {
		return events.Event{Type: events.ControllerStarted, Ts: ts, Subject: "gc", Actor: "controller"}
	}

	cases := []struct {
		name        string
		evts        []events.Event
		wantDetails []string
	}{
		{
			name: "[C,S,F,F,F] keeps streak and declaration",
			evts: []events.Event{
				completed(now.Add(-5 * time.Hour)), declared(now.Add(-5 * time.Hour)),
				failed(now.Add(-3 * time.Hour)), failed(now.Add(-2 * time.Hour)), failed(now.Add(-time.Hour)),
			},
			wantDetails: []string{`export-sweep: 3 consecutive failures, last "exit status 1"`, declDetail},
		},
		{
			name: "[C,F,F,F,S] declaration does not reset the streak",
			evts: []events.Event{
				completed(now.Add(-5 * time.Hour)), failed(now.Add(-4 * time.Hour)), failed(now.Add(-3 * time.Hour)),
				failed(now.Add(-2 * time.Hour)), declared(now.Add(-time.Hour)),
			},
			wantDetails: []string{`export-sweep: 3 consecutive failures, last "exit status 1"`, declDetail},
		},
		{
			name: "[C,S,C,S,F] keeps only the newest declaration",
			evts: []events.Event{
				completed(now.Add(-5 * time.Hour)), older(now.Add(-5 * time.Hour)),
				completed(now.Add(-3 * time.Hour)), declared(now.Add(-3 * time.Hour)), failed(now.Add(-time.Hour)),
			},
			wantDetails: []string{"export-sweep: 1 consecutive failure(s), under threshold 3", declDetail},
		},
		{
			name: "grace-skipped failure keeps its grace detail",
			evts: []events.Event{
				completed(now.Add(-5 * time.Hour)), declared(now.Add(-5 * time.Hour)),
				started(now.Add(-time.Hour)), failed(now.Add(-time.Hour + time.Minute)),
			},
			wantDetails: []string{"export-sweep: 1 recent failure(s) within controller-start grace window", declDetail},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cityPath, cfg := orderFiringTestCity(t)
			writeOrderFiringTestOrder(t, cityPath, "export-sweep", "cooldown", "6h")
			writeOrderFiringTestEvents(t, cityPath, tc.evts...)

			result := NewOrderOutcomeHealthyCheck(cfg, cityPath).Run(&CheckContext{CityPath: cityPath})

			if result.Status != StatusWarning {
				t.Fatalf("status = %v, want StatusWarning; msg=%q details=%v", result.Status, result.Message, result.Details)
			}
			if result.Severity != SeverityAdvisory {
				t.Fatalf("severity = %v, want SeverityAdvisory", result.Severity)
			}
			if strings.Join(result.Details, "\n") != strings.Join(tc.wantDetails, "\n") {
				t.Fatalf("details = %q, want %q", result.Details, tc.wantDetails)
			}
		})
	}
}

// TestOrderOutcomeHealthyIncompleteOutcomeOrderScope pins which declarations
// count: only monitored cron/cooldown orders outside suspended rigs, matched by
// full scoped subject, so a same-named order in another scope neither inherits
// nor clears the declaration, and the hint keeps the rig-aware target.
func TestOrderOutcomeHealthyIncompleteOutcomeOrderScope(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	setup := func(t *testing.T) (string, *config.City) {
		t.Helper()
		cityPath, cfg := orderFiringTestCity(t)
		ordersDir := filepath.Join(cityPath, "orders")
		writeOrderFiringTestOrderInDir(t, ordersDir, "export-sweep", "cooldown", "6h")
		writeOrderFiringTestOrderInDir(t, ordersDir, "manual-export", "manual", "")
		writeOrderFiringTestOrderInDir(t, ordersDir, "event-export", "event", "bead.closed")

		rigFormulas := map[string][]string{}
		for _, rig := range []config.Rig{
			{Name: "st", Path: filepath.Join(cityPath, "rigs", "st")},
			{Name: "parked", Path: filepath.Join(cityPath, "rigs", "parked"), SuspendedOnStart: true},
		} {
			rigOrders := filepath.Join(rig.Path, "orders")
			if err := os.MkdirAll(rigOrders, 0o755); err != nil {
				t.Fatalf("creating rig orders dir: %v", err)
			}
			cfg.Rigs = append(cfg.Rigs, rig)
			rigFormulas[rig.Name] = []string{cfg.FormulaLayers.City[0], filepath.Join(rig.Path, "formulas")}
		}
		cfg.FormulaLayers.Rigs = rigFormulas
		writeOrderFiringTestOrderInDir(t, filepath.Join(cityPath, "rigs", "st", "orders"), "export-sweep", "cron", "0 3 * * *")
		writeOrderFiringTestOrderInDir(t, filepath.Join(cityPath, "rigs", "parked", "orders"), "parked-sweep", "cooldown", "1h")
		return cityPath, cfg
	}
	declared := func(subject string, ts time.Time) events.Event {
		return skippedOrderEvent(subject, ts, "partial", "store unreachable",
			events.OrderSkippedScope{Scope: "rig:st", Reason: "dolt not reachable"})
	}
	completed := func(subject string, ts time.Time) events.Event {
		return events.Event{Type: events.OrderCompleted, Ts: ts, Subject: subject, Actor: "controller"}
	}
	excluded := []events.Event{
		declared("manual-export", now.Add(-30*time.Minute)),
		declared("event-export", now.Add(-30*time.Minute)),
		declared("parked-sweep:rig:parked", now.Add(-30*time.Minute)),
		declared("export-sweep:rig:other", now.Add(-30*time.Minute)),
	}

	cases := []struct {
		name        string
		evts        []events.Event
		wantDetails []string
		wantHint    string
	}{
		{
			name: "rig-scoped declaration survives a city-scope completion",
			evts: append([]events.Event{
				completed("export-sweep:rig:st", now.Add(-2*time.Hour)),
				declared("export-sweep:rig:st", now.Add(-2*time.Hour)),
				completed("export-sweep", now.Add(-time.Hour)),
			}, excluded...),
			wantDetails: []string{
				"export-sweep: last run succeeded",
				"export-sweep:rig:st: recorded incomplete work: partial: store unreachable; not run: rig:st (dolt not reachable)",
			},
			wantHint: "Inspect with: gc order check && gc order history export-sweep --rig st",
		},
		{
			name: "city-scoped declaration survives a rig-scope completion",
			evts: append([]events.Event{
				completed("export-sweep", now.Add(-2*time.Hour)),
				declared("export-sweep", now.Add(-2*time.Hour)),
				completed("export-sweep:rig:st", now.Add(-time.Hour)),
			}, excluded...),
			wantDetails: []string{
				"export-sweep: recorded incomplete work: partial: store unreachable; not run: rig:st (dolt not reachable)",
				"export-sweep:rig:st: last run succeeded",
			},
			wantHint: "Inspect with: gc order check && gc order history export-sweep",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cityPath, cfg := setup(t)
			writeOrderFiringTestEvents(t, cityPath, tc.evts...)

			result := NewOrderOutcomeHealthyCheck(cfg, cityPath).Run(&CheckContext{CityPath: cityPath})

			if result.Status != StatusWarning {
				t.Fatalf("status = %v, want StatusWarning; msg=%q details=%v", result.Status, result.Message, result.Details)
			}
			got := append([]string(nil), result.Details...)
			sort.Strings(got)
			if strings.Join(got, "\n") != strings.Join(tc.wantDetails, "\n") {
				t.Fatalf("details = %q, want %q (excluded orders and scopes must not appear)", got, tc.wantDetails)
			}
			if result.FixHint != tc.wantHint {
				t.Fatalf("FixHint = %q, want %q", result.FixHint, tc.wantHint)
			}
		})
	}
}

// TestOrderOutcomeHealthyMixedOutcomeSummary pins the aggregate: each unhealthy
// order counts once, an aggregate covering incomplete work replaces "failing
// repeatedly" whenever a declaration is outstanding, and a failure-only log
// keeps its exact existing output.
func TestOrderOutcomeHealthyMixedOutcomeSummary(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	failures := func(subject string) []events.Event {
		out := make([]events.Event, 0, 3)
		for i := 3; i >= 1; i-- {
			out = append(out, events.Event{
				Type: events.OrderFailed, Ts: now.Add(-time.Duration(i) * time.Hour), Subject: subject,
				Actor: "controller", Message: "exit status 1",
			})
		}
		return out
	}
	declared := func(subject string) events.Event {
		return skippedOrderEvent(subject, now.Add(-30*time.Minute), "skipped", "nothing to export")
	}

	cases := []struct {
		name        string
		evts        []events.Event
		wantMessage string
		wantDetails []string
		wantHint    string
	}{
		{
			name:        "failure-only log keeps existing output",
			evts:        failures("alpha-sweep"),
			wantMessage: "1 order(s) failing repeatedly",
			wantDetails: []string{`alpha-sweep: 3 consecutive failures, last "exit status 1"`, "beta-sweep: no completed runs yet"},
			wantHint:    "Inspect with: gc order check && gc order history alpha-sweep",
		},
		{
			name:        "declaration only",
			evts:        []events.Event{declared("beta-sweep")},
			wantMessage: "1 order(s) have unhealthy outcomes",
			wantDetails: []string{"alpha-sweep: no completed runs yet", "beta-sweep: recorded incomplete work: skipped: nothing to export"},
			wantHint:    "Inspect with: gc order check && gc order history beta-sweep",
		},
		{
			name:        "failing order and declaring order count separately",
			evts:        append(failures("alpha-sweep"), declared("beta-sweep")),
			wantMessage: "2 order(s) have unhealthy outcomes",
			wantDetails: []string{
				`alpha-sweep: 3 consecutive failures, last "exit status 1"`,
				"beta-sweep: recorded incomplete work: skipped: nothing to export",
			},
			wantHint: "Inspect with: gc order check && gc order history alpha-sweep",
		},
		{
			name:        "both signals on one order count it once",
			evts:        append([]events.Event{declared("alpha-sweep")}, failures("alpha-sweep")...),
			wantMessage: "1 order(s) have unhealthy outcomes",
			wantDetails: []string{
				`alpha-sweep: 3 consecutive failures, last "exit status 1"`,
				"alpha-sweep: recorded incomplete work: skipped: nothing to export",
				"beta-sweep: no completed runs yet",
			},
			wantHint: "Inspect with: gc order check && gc order history alpha-sweep",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cityPath, cfg := orderFiringTestCity(t)
			writeOrderFiringTestOrder(t, cityPath, "alpha-sweep", "cooldown", "1h")
			writeOrderFiringTestOrder(t, cityPath, "beta-sweep", "cron", "0 * * * *")
			writeOrderFiringTestEvents(t, cityPath, tc.evts...)

			result := NewOrderOutcomeHealthyCheck(cfg, cityPath).Run(&CheckContext{CityPath: cityPath})

			if result.Status != StatusWarning || result.Severity != SeverityAdvisory {
				t.Fatalf("status/severity = %v/%v, want StatusWarning/SeverityAdvisory; details=%v", result.Status, result.Severity, result.Details)
			}
			if result.Message != tc.wantMessage {
				t.Fatalf("message = %q, want %q", result.Message, tc.wantMessage)
			}
			if strings.Join(result.Details, "\n") != strings.Join(tc.wantDetails, "\n") {
				t.Fatalf("details = %q, want %q", result.Details, tc.wantDetails)
			}
			if result.FixHint != tc.wantHint {
				t.Fatalf("FixHint = %q, want %q", result.FixHint, tc.wantHint)
			}
		})
	}
}

// TestOrderOutcomeHealthySuppressedAndInapplicableLeaveDeclarations pins that
// order.suppressed and order.inapplicable events stay outside the declaration
// walk: they neither clear an outstanding declaration nor create one.
func TestOrderOutcomeHealthySuppressedAndInapplicableLeaveDeclarations(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name        string
		evts        []events.Event
		wantStatus  CheckStatus
		wantDetails []string
	}{
		{
			name: "suppressed events neither clear nor create declarations",
			evts: []events.Event{
				{Type: events.OrderCompleted, Ts: now.Add(-3 * time.Hour), Subject: "export-sweep", Actor: "controller"},
				{Type: events.OrderSuppressed, Ts: now.Add(-2 * time.Hour), Subject: "export-sweep", Actor: "controller", Message: "open-work gate held"},
				skippedOrderEvent("export-sweep", now.Add(-90*time.Minute), "partial", "store unreachable"),
				{Type: events.OrderSuppressed, Ts: now.Add(-time.Hour), Subject: "export-sweep", Actor: "controller", Message: "open-work gate held"},
			},
			wantStatus:  StatusWarning,
			wantDetails: []string{"export-sweep: recorded incomplete work: partial: store unreachable"},
		},
		{
			name: "inapplicable events do not create declarations",
			evts: []events.Event{
				{Type: events.OrderInapplicable, Ts: now.Add(-time.Hour), Subject: "export-sweep", Actor: "controller", Message: "requires backend postgres"},
			},
			wantStatus:  StatusOK,
			wantDetails: []string{"export-sweep: no completed runs yet"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cityPath, cfg := orderFiringTestCity(t)
			writeOrderFiringTestOrder(t, cityPath, "export-sweep", "cooldown", "6h")
			writeOrderFiringTestEvents(t, cityPath, tc.evts...)

			result := NewOrderOutcomeHealthyCheck(cfg, cityPath).Run(&CheckContext{CityPath: cityPath})

			if result.Status != tc.wantStatus {
				t.Fatalf("status = %v, want %v; msg=%q details=%v", result.Status, tc.wantStatus, result.Message, result.Details)
			}
			if !reflect.DeepEqual(result.Details, tc.wantDetails) {
				t.Fatalf("details = %v, want %v", result.Details, tc.wantDetails)
			}
		})
	}
}
