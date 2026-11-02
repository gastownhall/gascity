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

// TestOrderOutcomeHealthyRecordedIncompleteOutcomes pins the PR #8543 defect:
// an exec order can exit 0 while declaring that some of its work did not run
// (order.skipped), yet order-outcome-healthy read only completed/failed
// events, so the run surfaced as "last run succeeded" and the declaration
// stayed invisible. Events are recorded in log order — the recorder assigns
// Seq in record order — so each case's slice is its Seq order.
func TestOrderOutcomeHealthyRecordedIncompleteOutcomes(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	const subject = "export-sweep"
	const declMsg = "partial: store unreachable; not run: rig:st (dolt not reachable)"
	const olderMsg = "skipped: nothing ran; not run: rig:st (asleep)"
	const unreadableMsg = "unreadable outcome declaration: unexpected end of JSON input"

	completed := func(ts time.Time) events.Event {
		return events.Event{Type: events.OrderCompleted, Ts: ts, Subject: subject, Actor: "controller"}
	}
	declared := func(ts time.Time, msg string) events.Event {
		return events.Event{
			Type: events.OrderSkipped, Ts: ts, Subject: subject, Actor: "controller", Message: msg,
			Payload: events.OrderSkippedPayloadJSON(events.OrderSkippedPayload{
				OrderName: subject, Outcome: "partial", Reason: "store unreachable",
				Scopes: []events.OrderSkippedScope{{Scope: "rig:st", Reason: "dolt not reachable"}},
			}),
		}
	}
	failed := func(ts time.Time, msg string) events.Event {
		return events.Event{Type: events.OrderFailed, Ts: ts, Subject: subject, Actor: "controller", Message: msg}
	}

	cases := []struct {
		name       string
		evts       []events.Event
		wantStatus CheckStatus
		wantDetail []string
		notDetail  []string
	}{
		{
			name:       "completion then declaration warns",
			evts:       []events.Event{completed(now.Add(-time.Hour)), declared(now.Add(-time.Hour), declMsg)},
			wantStatus: StatusWarning,
			wantDetail: []string{"recorded incomplete work", declMsg},
			notDetail:  []string{"last run succeeded"},
		},
		{
			// Two overlapping completions, then two declarations: the newest
			// declaration is the recorded evidence. No execution is paired or
			// counted — the sequence alone cannot attribute either declaration
			// to a specific run.
			name: "newest of repeated declarations wins without counting executions",
			evts: []events.Event{
				completed(now.Add(-4 * time.Hour)),
				completed(now.Add(-3 * time.Hour)),
				declared(now.Add(-2*time.Hour), olderMsg),
				declared(now.Add(-time.Hour), declMsg),
			},
			wantStatus: StatusWarning,
			wantDetail: []string{"recorded incomplete work", declMsg},
			notDetail:  []string{olderMsg},
		},
		{
			name:       "declaration without any completion warns",
			evts:       []events.Event{declared(now.Add(-time.Hour), declMsg)},
			wantStatus: StatusWarning,
			wantDetail: []string{"recorded incomplete work", declMsg},
			notDetail:  []string{"no completed runs yet", "last run succeeded"},
		},
		{
			// A declaration with no message still means work did not run; the
			// detail must not grow a dangling ": " artifact.
			name:       "blank declaration message still warns generically",
			evts:       []events.Event{completed(now.Add(-time.Hour)), declared(now.Add(-time.Hour), "")},
			wantStatus: StatusWarning,
			wantDetail: []string{"recorded incomplete work"},
			notDetail:  []string{"last run succeeded", "recorded incomplete work: "},
		},
		{
			// The controller records a declaration it could not decode with the
			// decode error in the message; that text must reach the operator.
			name:       "unreadable declaration keeps its error text",
			evts:       []events.Event{completed(now.Add(-time.Hour)), declared(now.Add(-time.Hour), unreadableMsg)},
			wantStatus: StatusWarning,
			wantDetail: []string{"recorded incomplete work", unreadableMsg},
			notDetail:  []string{"last run succeeded"},
		},
		{
			// Seq, not Ts, orders the log: a declaration recorded after the
			// completion warns even when its timestamp is older.
			name:       "declaration with older timestamp but newer seq warns",
			evts:       []events.Event{completed(now), declared(now.Add(-2*time.Hour), declMsg)},
			wantStatus: StatusWarning,
			wantDetail: []string{"recorded incomplete work", declMsg},
			notDetail:  []string{"last run succeeded"},
		},
		{
			// A completion recorded after the declaration clears the warning
			// and the existing success detail returns.
			name:       "declaration cleared by later completion",
			evts:       []events.Event{completed(now.Add(-3 * time.Hour)), declared(now.Add(-2*time.Hour), declMsg), completed(now.Add(-time.Hour))},
			wantStatus: StatusOK,
			wantDetail: []string{"last run succeeded"},
			notDetail:  []string{"recorded incomplete work"},
		},
		{
			// [C1,S1,C2]: the clearing completion need not be the companion of
			// the declaring run; any later completion clears without either
			// execution being attributed.
			name:       "declaration cleared by any later completion",
			evts:       []events.Event{completed(now.Add(-3 * time.Hour)), declared(now.Add(-2*time.Hour), declMsg), completed(now.Add(-time.Hour)), completed(now.Add(-30 * time.Minute))},
			wantStatus: StatusOK,
			wantDetail: []string{"last run succeeded"},
			notDetail:  []string{"recorded incomplete work"},
		},
		{
			// Seq, not Ts, governs clearing too: a completion with an older
			// timestamp but newer Seq clears the declaration.
			name:       "completion with older timestamp but newer seq clears",
			evts:       []events.Event{declared(now, declMsg), completed(now.Add(-2 * time.Hour))},
			wantStatus: StatusOK,
			wantDetail: []string{"last run succeeded"},
			notDetail:  []string{"recorded incomplete work"},
		},
		{
			// [C,S,F]: the newest declaration and the failure detail are both
			// diagnostics of the same order — a failure is not the clearing
			// event, so neither may drop the other.
			name:       "declaration with under-threshold failure keeps both diagnostics",
			evts:       []events.Event{completed(now.Add(-4 * time.Hour)), declared(now.Add(-3*time.Hour), declMsg), failed(now.Add(-2*time.Hour), "exit status 1")},
			wantStatus: StatusWarning,
			wantDetail: []string{"1 consecutive failure(s), under threshold 3", "recorded incomplete work", declMsg},
			notDetail:  []string{"last run succeeded"},
		},
		{
			// [C,S,F,F,F]: an at-threshold failure streak and the declaration
			// both surface.
			name: "declaration with threshold failure streak keeps both diagnostics",
			evts: []events.Event{
				completed(now.Add(-5 * time.Hour)),
				declared(now.Add(-4*time.Hour), declMsg),
				failed(now.Add(-3*time.Hour), "exit status 1"),
				failed(now.Add(-2*time.Hour), "exit status 1"),
				failed(now.Add(-time.Hour), "exit status 1"),
			},
			wantStatus: StatusWarning,
			wantDetail: []string{"3 consecutive failures", "recorded incomplete work", declMsg},
			notDetail:  []string{"last run succeeded"},
		},
		{
			// [C,F,F,F,S]: a declaration recorded after the failures keeps the
			// failure streak intact — declarations neither count nor reset.
			name: "declaration after failure streak keeps the streak",
			evts: []events.Event{
				completed(now.Add(-5 * time.Hour)),
				failed(now.Add(-4*time.Hour), "exit status 1"),
				failed(now.Add(-3*time.Hour), "exit status 1"),
				failed(now.Add(-2*time.Hour), "exit status 1"),
				declared(now.Add(-time.Hour), declMsg),
			},
			wantStatus: StatusWarning,
			wantDetail: []string{"3 consecutive failures", "recorded incomplete work", declMsg},
			notDetail:  []string{"last run succeeded"},
		},
		{
			// The controller-start grace suppresses spurious FAILURES, not
			// declarations: evidence that work did not run is never grace-
			// skipped.
			name: "declaration inside startup grace still warns",
			evts: []events.Event{
				{Type: events.ControllerStarted, Ts: now.Add(-5 * time.Minute), Subject: "gc", Actor: "controller"},
				completed(now.Add(-4 * time.Minute)),
				declared(now.Add(-3*time.Minute), declMsg),
			},
			wantStatus: StatusWarning,
			wantDetail: []string{"recorded incomplete work", declMsg},
			notDetail:  []string{"last run succeeded"},
		},
		{
			// Grace-skipped failures and the declaration are both reported:
			// "recent failure(s) within grace" stays truthful alongside the
			// declaration.
			name: "declaration with grace-skipped failures keeps both diagnostics",
			evts: []events.Event{
				{Type: events.ControllerStarted, Ts: now.Add(-5 * time.Minute), Subject: "gc", Actor: "controller"},
				completed(now.Add(-4 * time.Minute)),
				failed(now.Add(-3*time.Minute), "context canceled"),
				declared(now.Add(-2*time.Minute), declMsg),
			},
			wantStatus: StatusWarning,
			wantDetail: []string{"1 recent failure(s) within controller-start grace window", "recorded incomplete work", declMsg},
			notDetail:  []string{"last run succeeded"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cityPath, cfg := orderFiringTestCity(t)
			writeOrderFiringTestOrderInDir(t, filepath.Join(cityPath, "orders"), subject, "cooldown", "6h")
			writeOrderFiringTestEvents(t, cityPath, tc.evts...)

			result := NewOrderOutcomeHealthyCheck(cfg, cityPath).Run(&CheckContext{CityPath: cityPath})

			if result.Status != tc.wantStatus {
				t.Fatalf("status = %v, want %v; msg=%q details=%v", result.Status, tc.wantStatus, result.Message, result.Details)
			}
			if result.Severity != SeverityAdvisory {
				t.Fatalf("severity = %v, want SeverityAdvisory", result.Severity)
			}
			joined := strings.Join(result.Details, "\n")
			for _, want := range tc.wantDetail {
				if !strings.Contains(joined, want) {
					t.Fatalf("details missing %q:\n%s", want, joined)
				}
			}
			for _, notWant := range tc.notDetail {
				if strings.Contains(joined, notWant) {
					t.Fatalf("details must not contain %q:\n%s", notWant, joined)
				}
			}
		})
	}
}

// TestLatestIncompleteOrderOutcome pins the declaration walk directly: it
// returns the newest order.skipped for the subject, ignoring other subjects'
// events, stopping without a declaration at a completion, and walking across
// failures (a failure is not the clearing event).
func TestLatestIncompleteOrderOutcome(t *testing.T) {
	base := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	outcomes := []events.Event{
		outcomeEvent(1, "export-sweep", events.OrderSkipped, base, "older declaration"),
		outcomeEvent(2, "other-order", events.OrderSkipped, base.Add(time.Hour), "not mine"),
		outcomeEvent(3, "export-sweep", events.OrderSkipped, base.Add(2*time.Hour), "newest declaration"),
	}

	decl, ok := latestIncompleteOrderOutcome(outcomes, "export-sweep")

	if !ok {
		t.Fatal("ok = false, want a declaration for export-sweep")
	}
	if decl.Message != "newest declaration" {
		t.Fatalf("message = %q, want the newest declaration", decl.Message)
	}

	if _, ok := latestIncompleteOrderOutcome(outcomes, "unheard-of"); ok {
		t.Fatal("ok = true for a subject with no events, want false")
	}
	if _, ok := latestIncompleteOrderOutcome(nil, "export-sweep"); ok {
		t.Fatal("ok = true for an empty stream, want false")
	}

	// A completion recorded after the declaration clears it.
	cleared := []events.Event{
		outcomeEvent(1, "export-sweep", events.OrderCompleted, base, ""),
		outcomeEvent(2, "export-sweep", events.OrderSkipped, base.Add(time.Hour), "partial"),
		outcomeEvent(3, "export-sweep", events.OrderCompleted, base.Add(2*time.Hour), ""),
	}
	if _, ok := latestIncompleteOrderOutcome(cleared, "export-sweep"); ok {
		t.Fatal("ok = true for a declaration followed by a completion, want false — the completion clears it")
	}

	// Failures after the declaration are transparent: the declaration stays
	// outstanding, and the walk crosses the failures to reach it.
	acrossFailures := []events.Event{
		outcomeEvent(1, "export-sweep", events.OrderCompleted, base, ""),
		outcomeEvent(2, "export-sweep", events.OrderSkipped, base.Add(time.Hour), "partial: rig asleep"),
		outcomeEvent(3, "export-sweep", events.OrderFailed, base.Add(2*time.Hour), "exit status 1"),
		outcomeEvent(4, "export-sweep", events.OrderFailed, base.Add(3*time.Hour), "exit status 1"),
	}
	decl, ok = latestIncompleteOrderOutcome(acrossFailures, "export-sweep")
	if !ok || decl.Message != "partial: rig asleep" {
		t.Fatalf("decl/ok = %q/%v, want the declaration kept across later failures", decl.Message, ok)
	}

	// An empty-message declaration still reports ok=true: work did not run.
	blank := []events.Event{
		outcomeEvent(1, "export-sweep", events.OrderCompleted, base, ""),
		outcomeEvent(2, "export-sweep", events.OrderSkipped, base.Add(time.Hour), ""),
	}
	if decl, ok = latestIncompleteOrderOutcome(blank, "export-sweep"); !ok || decl.Message != "" {
		t.Fatalf("decl/ok = %q/%v, want an empty-message declaration with ok=true", decl.Message, ok)
	}
}

// TestConsecutiveOrderFailuresIgnoresDeclarations pins that order.skipped
// declarations are invisible to the failure walk: all four outputs must equal
// those for the same stream with the declarations removed. A declaration is
// neither success nor failure — counting it as either would let a partial run
// reset or extend the hard-failure streak.
func TestConsecutiveOrderFailuresIgnoresDeclarations(t *testing.T) {
	base := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	start := base.Add(10 * time.Hour)

	strip := func(outcomes []events.Event) []events.Event {
		kept := make([]events.Event, 0, len(outcomes))
		for _, e := range outcomes {
			if e.Type != events.OrderSkipped {
				kept = append(kept, e)
			}
		}
		return kept
	}

	cases := []struct {
		name     string
		outcomes []events.Event
		subject  string
		starts   []time.Time
	}{
		{
			// A declaration alone must not mark the order as having produced
			// an outcome: sawOutcome stays false, as for an empty stream.
			name:     "declaration only",
			outcomes: []events.Event{outcomeEvent(1, "export-sweep", events.OrderSkipped, base, "partial")},
			subject:  "export-sweep",
		},
		{
			name: "declaration between failures",
			outcomes: []events.Event{
				outcomeEvent(1, "export-sweep", events.OrderFailed, base, "exit status 1"),
				outcomeEvent(2, "export-sweep", events.OrderSkipped, base.Add(time.Hour), "partial"),
				outcomeEvent(3, "export-sweep", events.OrderFailed, base.Add(2*time.Hour), "exit status 1"),
				outcomeEvent(4, "export-sweep", events.OrderFailed, base.Add(3*time.Hour), "exit status 1"),
			},
			subject: "export-sweep",
		},
		{
			name: "declaration before success",
			outcomes: []events.Event{
				outcomeEvent(1, "export-sweep", events.OrderFailed, base, "exit status 1"),
				outcomeEvent(2, "export-sweep", events.OrderFailed, base.Add(time.Hour), "exit status 1"),
				outcomeEvent(3, "export-sweep", events.OrderSkipped, base.Add(2*time.Hour), "partial"),
				outcomeEvent(4, "export-sweep", events.OrderCompleted, base.Add(3*time.Hour), ""),
			},
			subject: "export-sweep",
		},
		{
			// Failures inside the grace window stay skipped whether or not a
			// declaration is interleaved; a completion inside the grace window
			// still ends the walk.
			name: "declaration interleaved with grace-window events",
			outcomes: []events.Event{
				outcomeEvent(1, "export-sweep", events.OrderCompleted, start.Add(-time.Hour), ""),
				outcomeEvent(2, "export-sweep", events.OrderFailed, start.Add(time.Minute), "context canceled"),
				outcomeEvent(3, "export-sweep", events.OrderSkipped, start.Add(2*time.Minute), "partial"),
				outcomeEvent(4, "export-sweep", events.OrderFailed, start.Add(3*time.Minute), "context canceled"),
			},
			subject: "export-sweep",
			starts:  []time.Time{start},
		},
		{
			name: "another subject's declaration",
			outcomes: []events.Event{
				outcomeEvent(1, "export-sweep", events.OrderFailed, base, "exit status 1"),
				outcomeEvent(2, "other-order", events.OrderSkipped, base.Add(time.Hour), "not mine"),
				outcomeEvent(3, "export-sweep", events.OrderFailed, base.Add(2*time.Hour), "exit status 1"),
			},
			subject: "export-sweep",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			streak, lastMessage, sawOutcome, skipped := consecutiveOrderFailures(tc.outcomes, tc.subject, tc.starts, 10*time.Minute)
			wantStreak, wantLastMessage, wantSawOutcome, wantSkipped := consecutiveOrderFailures(strip(tc.outcomes), tc.subject, tc.starts, 10*time.Minute)

			if streak != wantStreak || lastMessage != wantLastMessage || sawOutcome != wantSawOutcome || skipped != wantSkipped {
				t.Fatalf("outputs with declarations (%d, %q, %v, %d) differ from declarations-removed (%d, %q, %v, %d) — declarations must be streak-neutral",
					streak, lastMessage, sawOutcome, skipped, wantStreak, wantLastMessage, wantSawOutcome, wantSkipped)
			}
		})
	}

	// The declaration-only case must specifically leave sawOutcome false, not
	// merely match the stripped stream by accident of both seeing nothing.
	if _, _, sawOutcome, _ := consecutiveOrderFailures(
		[]events.Event{outcomeEvent(1, "export-sweep", events.OrderSkipped, base, "partial")},
		"export-sweep", nil, 10*time.Minute); sawOutcome {
		t.Fatal("sawOutcome = true for a declaration-only stream, want false")
	}
}

// TestOrderOutcomeHealthyIncompleteOutcomeOrderScope pins the scope rules for
// declaration warnings: only cron/cooldown orders of non-suspended rigs are
// eligible, subjects match on the full scoped name (so a same-named order in
// another rig neither warns nor clears another's declaration), and the
// inspection hint keeps its existing rig-aware target form.
func TestOrderOutcomeHealthyIncompleteOutcomeOrderScope(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	const declMsg = "partial: store unreachable; not run: rig:st (dolt not reachable)"

	declared := func(subject string) events.Event {
		return events.Event{
			Type: events.OrderSkipped, Ts: now.Add(-time.Hour), Subject: subject, Actor: "controller", Message: declMsg,
			Payload: events.OrderSkippedPayloadJSON(events.OrderSkippedPayload{
				OrderName: subject, Outcome: "partial", Reason: "store unreachable",
				Scopes: []events.OrderSkippedScope{{Scope: "rig:st", Reason: "dolt not reachable"}},
			}),
		}
	}
	completed := func(subject string, ts time.Time) events.Event {
		return events.Event{Type: events.OrderCompleted, Ts: ts, Subject: subject, Actor: "controller"}
	}

	// rigOrder wires one order into rig rigName, mirroring how a rig-scoped
	// order is discovered by the scan both checks share.
	rigOrder := func(t *testing.T, cityPath string, cfg *config.City, rigName, name, trigger, timing string) {
		t.Helper()
		rigPath := filepath.Join(cityPath, "rigs", rigName)
		rigOrders := filepath.Join(rigPath, "orders")
		if err := os.MkdirAll(rigOrders, 0o755); err != nil {
			t.Fatalf("creating rig orders dir: %v", err)
		}
		cfg.Rigs = append(cfg.Rigs, config.Rig{Name: rigName, Path: rigPath})
		if cfg.FormulaLayers.Rigs == nil {
			cfg.FormulaLayers.Rigs = make(map[string][]string)
		}
		cfg.FormulaLayers.Rigs[rigName] = append(cfg.FormulaLayers.Rigs[rigName], cfg.FormulaLayers.City[0], filepath.Join(rigPath, "formulas"))
		writeOrderFiringTestOrderInDir(t, rigOrders, name, trigger, timing)
	}

	t.Run("rig-scoped order warns with rig-aware hint", func(t *testing.T) {
		cityPath, cfg := orderFiringTestCity(t)
		rigOrder(t, cityPath, cfg, "st", "export-sweep", "cooldown", "6h")
		writeOrderFiringTestEvents(t, cityPath,
			completed("export-sweep:rig:st", now.Add(-2*time.Hour)),
			declared("export-sweep:rig:st"),
		)

		result := NewOrderOutcomeHealthyCheck(cfg, cityPath).Run(&CheckContext{CityPath: cityPath})

		if result.Status != StatusWarning {
			t.Fatalf("status = %v, want StatusWarning; msg=%q details=%v", result.Status, result.Message, result.Details)
		}
		wantHint := "Inspect with: gc order check && gc order history export-sweep --rig st"
		if result.FixHint != wantHint {
			t.Fatalf("FixHint = %q, want %q — the hint must keep the rig-aware target form", result.FixHint, wantHint)
		}
		if joined := strings.Join(result.Details, "\n"); !strings.Contains(joined, declMsg) {
			t.Fatalf("details = %v, want the declaration message for the scoped subject", result.Details)
		}
	})

	t.Run("city-scope order hint stays bare", func(t *testing.T) {
		cityPath, cfg := orderFiringTestCity(t)
		writeOrderFiringTestOrderInDir(t, filepath.Join(cityPath, "orders"), "export-sweep", "cron", "*/5 * * * *")
		writeOrderFiringTestEvents(t, cityPath,
			completed("export-sweep", now.Add(-2*time.Hour)),
			declared("export-sweep"),
		)

		result := NewOrderOutcomeHealthyCheck(cfg, cityPath).Run(&CheckContext{CityPath: cityPath})

		if result.Status != StatusWarning {
			t.Fatalf("status = %v, want StatusWarning", result.Status)
		}
		wantHint := "Inspect with: gc order check && gc order history export-sweep"
		if result.FixHint != wantHint {
			t.Fatalf("FixHint = %q, want %q", result.FixHint, wantHint)
		}
	})

	t.Run("other-scope completion does not clear the declaration", func(t *testing.T) {
		cityPath, cfg := orderFiringTestCity(t)
		rigOrder(t, cityPath, cfg, "st", "export-sweep", "cooldown", "6h")
		rigOrder(t, cityPath, cfg, "other", "export-sweep", "cooldown", "6h")
		writeOrderFiringTestEvents(t, cityPath,
			completed("export-sweep:rig:st", now.Add(-3*time.Hour)),
			declared("export-sweep:rig:st"),
			// A completion for the same order name in another rig, recorded
			// after the declaration: subjects are scoped, so it clears nothing.
			completed("export-sweep:rig:other", now.Add(-time.Hour)),
		)

		result := NewOrderOutcomeHealthyCheck(cfg, cityPath).Run(&CheckContext{CityPath: cityPath})

		if result.Status != StatusWarning {
			t.Fatalf("status = %v, want StatusWarning — an other-scope completion must not clear the declaration", result.Status)
		}
		if result.FixHint != "Inspect with: gc order check && gc order history export-sweep --rig st" {
			t.Fatalf("FixHint = %q, want the declaring rig's hint", result.FixHint)
		}
	})

	t.Run("manual and event orders stay excluded", func(t *testing.T) {
		cityPath, cfg := orderFiringTestCity(t)
		ordersDir := filepath.Join(cityPath, "orders")
		writeOrderFiringTestOrderInDir(t, ordersDir, "ticket-intake", "manual", "")
		writeOrderFiringTestOrderInDir(t, ordersDir, "on-event", "event", "bead.created")
		writeOrderFiringTestEvents(t, cityPath,
			declared("ticket-intake"),
			declared("on-event"),
		)

		result := NewOrderOutcomeHealthyCheck(cfg, cityPath).Run(&CheckContext{CityPath: cityPath})

		if result.Status != StatusOK {
			t.Fatalf("status = %v, want StatusOK — manual and event orders are out of scope by construction", result.Status)
		}
		if joined := strings.Join(result.Details, "\n"); strings.Contains(joined, declMsg) {
			t.Fatalf("details must not carry declarations of excluded orders:\n%s", joined)
		}
	})

	t.Run("suspended rig order stays excluded", func(t *testing.T) {
		cityPath, cfg := orderFiringTestCity(t)
		rigOrder(t, cityPath, cfg, "st", "export-sweep", "cooldown", "6h")
		cfg.Rigs[len(cfg.Rigs)-1].SuspendedOnStart = true
		writeOrderFiringTestEvents(t, cityPath,
			completed("export-sweep:rig:st", now.Add(-2*time.Hour)),
			declared("export-sweep:rig:st"),
		)

		result := NewOrderOutcomeHealthyCheck(cfg, cityPath).Run(&CheckContext{CityPath: cityPath})

		if result.Status != StatusOK {
			t.Fatalf("status = %v, want StatusOK — a suspended rig's order is out of scope", result.Status)
		}
		if joined := strings.Join(result.Details, "\n"); strings.Contains(joined, declMsg) {
			t.Fatalf("details must not carry declarations of suspended rigs:\n%s", joined)
		}
	})
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
