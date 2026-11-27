package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNotifyOnOrderFailingOrders pins both orders' event contracts: each
// wakes on one of the controller's order-streak events and runs the one
// script.
func TestNotifyOnOrderFailingOrders(t *testing.T) {
	assertEventExecOrder(t, "notify-on-order-failing.toml", "order.failing", "notify-order-failing.sh")
	assertEventExecOrder(t, "notify-on-order-recovered.toml", "order.recovered", "notify-order-failing.sh")
}

const notifyOrderFailingScriptPath = "assets/scripts/notify-order-failing.sh"

const (
	gateSweepFailingEvent   = `{"seq":201,"type":"order.failing","subject":"gate-sweep","message":"order gate-sweep has failed 3 times in a row since 2026-11-20T08:00:00Z: exit status 1","payload":{"order_name":"gate-sweep","consecutive":3,"first_failed":"2026-11-20T08:00:00Z","failing_for_ms":120000,"last_error":"exit status 1"}}`
	gateSweepRecoveredEvent = `{"seq":260,"type":"order.recovered","subject":"gate-sweep","message":"order gate-sweep recovered after 5 consecutive failures since 2026-11-20T08:00:00Z","payload":{"order_name":"gate-sweep","consecutive":5,"first_failed":"2026-11-20T08:00:00Z","failed_for_ms":3600000}}`
	selfFailingEvent        = `{"seq":205,"type":"order.failing","subject":"notify-on-order-failing","message":"order notify-on-order-failing has failed 3 times in a row","payload":{"order_name":"notify-on-order-failing","consecutive":3,"first_failed":"2026-11-20T08:10:00Z","failing_for_ms":60000,"last_error":"exit status 1"}}`
	// selfRigFailingEvent is the notify order alerting on itself under the
	// :rig:<rig> ScopedName a rig-scoped order runs as.
	selfRigFailingEvent = `{"seq":206,"type":"order.failing","subject":"notify-on-order-failing:rig:x","message":"order notify-on-order-failing:rig:x has failed 3 times in a row","payload":{"order_name":"notify-on-order-failing:rig:x","consecutive":3,"first_failed":"2026-11-20T08:10:00Z","failing_for_ms":60000,"last_error":"exit status 1"}}`
	// gateSweepRealertEvent is the same streak's hourly re-alert: same
	// first_failed, a longer streak.
	gateSweepRealertEvent = `{"seq":240,"type":"order.failing","subject":"gate-sweep","message":"order gate-sweep has failed 4 times in a row since 2026-11-20T08:00:00Z: exit status 1","payload":{"order_name":"gate-sweep","consecutive":4,"first_failed":"2026-11-20T08:00:00Z","failing_for_ms":3720000,"last_error":"exit status 1"}}`
	// gateSweepNextStreakEvent is the alert of a new streak that opened after
	// gateSweepRecoveredEvent ended the first one.
	gateSweepNextStreakEvent = `{"seq":300,"type":"order.failing","subject":"gate-sweep","message":"order gate-sweep has failed 3 times in a row since 2026-11-20T09:10:00Z: exit status 2","payload":{"order_name":"gate-sweep","consecutive":3,"first_failed":"2026-11-20T09:10:00Z","failing_for_ms":120000,"last_error":"exit status 2"}}`
	// doltHealthFailingEvent is another order's alert, between gate-sweep's.
	doltHealthFailingEvent = `{"seq":270,"type":"order.failing","subject":"dolt-health","message":"order dolt-health has failed 3 times in a row since 2026-11-20T09:00:00Z: exit status 1","payload":{"order_name":"dolt-health","consecutive":3,"first_failed":"2026-11-20T09:00:00Z","failing_for_ms":60000,"last_error":"exit status 1"}}`
)

// orderFailingRun is one notify-order-failing.sh run against a fake gc that
// answers each events read from a file, escalating through a stub that logs
// its argv, one line per call (newlines in a message flattened to spaces).
type orderFailingRun struct {
	work, binDir, escalateLog string
	env                       []string
}

func newOrderFailingRun(t *testing.T) *orderFailingRun {
	t.Helper()
	work := t.TempDir()
	binDir, _ := fakeGCBin(t, `case "$1 $2 $3" in
"events --type order.failing") cat "`+filepath.Join(work, "failing.jsonl")+`" ;;
"events --type order.recovered") cat "`+filepath.Join(work, "recovered.jsonl")+`" ;;
esac
`)
	escalateLog := filepath.Join(work, "escalate.log")
	stub := filepath.Join(work, "escalate.sh")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nprintf '%s\\n' \"$(printf '%s' \"$*\" | tr '\\n' ' ')\" >> \""+escalateLog+"\"\n"), 0o755); err != nil { //nolint:gosec // test stub must be executable
		t.Fatal(err)
	}
	return &orderFailingRun{
		work: work, binDir: binDir, escalateLog: escalateLog,
		env: []string{"GC_ESCALATE_SCRIPT=" + stub, "GC_CITY_PATH=" + work},
	}
}

func (r *orderFailingRun) run(t *testing.T, failing, recovered []string) []string {
	t.Helper()
	for name, lines := range map[string][]string{"failing.jsonl": failing, "recovered.jsonl": recovered} {
		if err := os.WriteFile(filepath.Join(r.work, name), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.Remove(r.escalateLog)
	if out, err := runPackScript(t, notifyOrderFailingScriptPath, r.binDir, escalationEnvKeys, r.env); err != nil {
		t.Fatalf("notify-order-failing: %v\n%s", err, out)
	}
	data, err := os.ReadFile(r.escalateLog)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var calls []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line != "" {
			calls = append(calls, line)
		}
	}
	return calls
}

// TestNotifyOrderFailingEscalatesTheStreakAndResolvesItOnRecovery is the
// script's contract: an order.failing escalates once as incident
// order-failing:<order>, deduplicated on the streak's first_failed; an
// order.recovered resolves that incident; and an alert about one of the
// notify orders themselves sends nothing, so a broken notifier cannot page
// about itself in a loop.
func TestNotifyOrderFailingEscalatesTheStreakAndResolvesItOnRecovery(t *testing.T) {
	r := newOrderFailingRun(t)

	calls := r.run(t, []string{gateSweepFailingEvent}, nil)
	if len(calls) != 1 {
		t.Fatalf("escalate.sh calls for one order.failing = %d, want 1: %q", len(calls), calls)
	}
	if !strings.Contains(calls[0], "--incident-key order-failing:gate-sweep --fingerprint 2026-11-20T08:00:00Z") {
		t.Errorf("escalate.sh call = %q, want incident key order-failing:gate-sweep fingerprinted by first_failed", calls[0])
	}
	if !strings.Contains(calls[0], "gate-sweep") || !strings.Contains(calls[0], "exit status 1") {
		t.Errorf("escalate.sh call = %q, want the order name and its last error", calls[0])
	}

	calls = r.run(t, []string{gateSweepFailingEvent}, []string{gateSweepRecoveredEvent})
	if len(calls) != 1 || calls[0] != "--resolve-incident order-failing:gate-sweep" {
		t.Fatalf("escalate.sh calls after order.recovered = %q, want exactly one resolve of order-failing:gate-sweep", calls)
	}

	for _, self := range []string{selfFailingEvent, selfRigFailingEvent} {
		if calls := r.run(t, []string{self}, nil); len(calls) != 0 {
			t.Fatalf("an order.failing about the notify order itself escalated: %q", calls)
		}
	}
}

// TestNotifyOrderFailingActsOnEachOrdersLatestEvent: a window holding several
// streaks of one order acts on the order's latest event alone, so the earlier
// streak's recovery neither resolves the incident of the streak that followed
// it nor stops another order's alert in between.
func TestNotifyOrderFailingActsOnEachOrdersLatestEvent(t *testing.T) {
	r := newOrderFailingRun(t)

	calls := r.run(t,
		[]string{gateSweepFailingEvent, gateSweepRealertEvent, doltHealthFailingEvent, gateSweepNextStreakEvent},
		[]string{gateSweepRecoveredEvent})
	if len(calls) != 2 {
		t.Fatalf("escalate.sh calls = %q, want dolt-health's alert then gate-sweep's new streak, nothing else", calls)
	}
	if !strings.Contains(calls[0], "--incident-key order-failing:dolt-health --fingerprint 2026-11-20T09:00:00Z") {
		t.Errorf("first escalate.sh call = %q, want dolt-health's streak", calls[0])
	}
	if !strings.Contains(calls[1], "--incident-key order-failing:gate-sweep --fingerprint 2026-11-20T09:10:00Z") {
		t.Errorf("second escalate.sh call = %q, want gate-sweep's new streak, fingerprinted by its own first_failed", calls[1])
	}
}

// TestNotifyOrderFailingMailsOncePerStreak runs two streaks of one order
// through the core escalate.sh: the streak mails the escalation recipient
// once, its hourly re-alert (same first_failed) and re-reads of the event
// window mail nothing more, the recovery resolves the incident without
// another mail, and the next streak mails once however often the window that
// still holds the first streak's recovery is re-read.
func TestNotifyOrderFailingMailsOncePerStreak(t *testing.T) {
	work := t.TempDir()
	binDir, logPath := fakeGCBin(t, `case "$1 $2 $3" in
"events --type order.failing") cat "`+filepath.Join(work, "failing.jsonl")+`" ;;
"events --type order.recovered") cat "`+filepath.Join(work, "recovered.jsonl")+`" ;;
"mail send human") echo "Sent message m-1 to human" ;;
esac
`)
	incidents := filepath.Join(work, "incidents")
	env := []string{
		"GC_ESCALATE_SCRIPT=" + coreEscalateScript(t),
		"GC_ESCALATE_INCIDENT_DIR=" + incidents,
		"GC_CITY_PATH=" + work,
	}
	run := func(failing, recovered []string) {
		t.Helper()
		for name, lines := range map[string][]string{"failing.jsonl": failing, "recovered.jsonl": recovered} {
			if err := os.WriteFile(filepath.Join(work, name), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if out, err := runPackScript(t, notifyOrderFailingScriptPath, binDir, escalationEnvKeys, env); err != nil {
			t.Fatalf("notify-order-failing: %v\n%s", err, out)
		}
	}
	incidentStates := func() []string {
		t.Helper()
		states, err := filepath.Glob(filepath.Join(incidents, "*.state"))
		if err != nil {
			t.Fatal(err)
		}
		return states
	}

	run([]string{gateSweepFailingEvent}, nil)
	sends := claimsStallMailSends(t, logPath)
	if len(sends) != 1 || !strings.Contains(sends[0], "gc mail send human ") || !strings.Contains(sends[0], "Order gate-sweep keeps failing") {
		t.Fatalf("sends after the alert = %q, want one mail to human naming the failing order", sends)
	}
	if states := incidentStates(); len(states) != 1 {
		t.Fatalf("incident records after the alert = %v, want the streak's one", states)
	}

	// The hourly re-alert and re-reads of the window mail nothing more.
	run([]string{gateSweepFailingEvent, gateSweepRealertEvent}, nil)
	run([]string{gateSweepFailingEvent, gateSweepRealertEvent}, nil)
	if sends := claimsStallMailSends(t, logPath); len(sends) != 1 {
		t.Fatalf("sends = %d after the re-alert and re-reads, want still 1:\n%s", len(sends), strings.Join(sends, "\n"))
	}

	// The recovery resolves the incident silently.
	run([]string{gateSweepFailingEvent, gateSweepRealertEvent}, []string{gateSweepRecoveredEvent})
	run([]string{gateSweepFailingEvent, gateSweepRealertEvent}, []string{gateSweepRecoveredEvent})
	if sends := claimsStallMailSends(t, logPath); len(sends) != 1 {
		t.Fatalf("sends = %d after the recovery, want still 1 (recovery resolves silently):\n%s", len(sends), strings.Join(sends, "\n"))
	}
	if states := incidentStates(); len(states) != 0 {
		t.Fatalf("incident records after the recovery = %v, want the streak's resolved", states)
	}

	// The order's next streak mails once, however often the window that
	// still holds the first streak's recovery is replayed.
	for range 3 {
		run([]string{gateSweepFailingEvent, gateSweepRealertEvent, gateSweepNextStreakEvent}, []string{gateSweepRecoveredEvent})
	}
	sends = claimsStallMailSends(t, logPath)
	if len(sends) != 2 || !strings.Contains(sends[1], "since 2026-11-20T09:10:00Z") {
		t.Fatalf("sends after three runs over the next streak = %d, want 2 (one per streak):\n%s", len(sends), strings.Join(sends, "\n"))
	}
	if states := incidentStates(); len(states) != 1 {
		t.Fatalf("incident records after the next streak = %v, want its one", states)
	}
}
