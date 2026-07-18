package orders

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/processgroup/processgrouptest"
)

func neverRan(_ string) (time.Time, error) { return time.Time{}, nil }

func TestCheckTriggerCooldownNeverRun(t *testing.T) {
	a := Order{Name: "digest", Trigger: "cooldown", Interval: "24h"}
	now := time.Date(2026, 2, 27, 12, 0, 0, 0, time.UTC)
	result := CheckTrigger(a, now, neverRan, nil, nil)
	if !result.Due {
		t.Errorf("Due = false, want true (never run)")
	}
	if result.Reason != "never run" {
		t.Errorf("Reason = %q, want %q", result.Reason, "never run")
	}
}

func TestCheckTriggerCooldownDue(t *testing.T) {
	a := Order{Name: "digest", Trigger: "cooldown", Interval: "24h"}
	now := time.Date(2026, 2, 27, 12, 0, 0, 0, time.UTC)
	lastRun := now.Add(-25 * time.Hour) // 25h ago — past the 24h interval
	lastRunFn := func(_ string) (time.Time, error) { return lastRun, nil }

	result := CheckTrigger(a, now, lastRunFn, nil, nil)
	if !result.Due {
		t.Errorf("Due = false, want true (25h > 24h)")
	}
}

func TestCheckTriggerCooldownNotDue(t *testing.T) {
	a := Order{Name: "digest", Trigger: "cooldown", Interval: "24h"}
	now := time.Date(2026, 2, 27, 12, 0, 0, 0, time.UTC)
	lastRun := now.Add(-12 * time.Hour) // 12h ago — within 24h interval
	lastRunFn := func(_ string) (time.Time, error) { return lastRun, nil }

	result := CheckTrigger(a, now, lastRunFn, nil, nil)
	if result.Due {
		t.Errorf("Due = true, want false (12h < 24h)")
	}
}

func TestCheckTriggerManual(t *testing.T) {
	a := Order{Name: "deploy", Trigger: "manual"}
	now := time.Date(2026, 2, 27, 12, 0, 0, 0, time.UTC)
	result := CheckTrigger(a, now, neverRan, nil, nil)
	if result.Due {
		t.Errorf("Due = true, want false (manual never auto-fires)")
	}
}

func TestCheckTriggerCronMatched(t *testing.T) {
	a := Order{Name: "cleanup", Trigger: "cron", Schedule: "0 3 * * *"}
	// 03:00 UTC — should match.
	now := time.Date(2026, 2, 27, 3, 0, 0, 0, time.UTC)
	result := CheckTrigger(a, now, neverRan, nil, nil)
	if !result.Due {
		t.Errorf("Due = false, want true (schedule matches 03:00)")
	}
}

func TestCheckTriggerCronEveryMinuteStepMatched(t *testing.T) {
	a := Order{Name: "cleanup", Trigger: "cron", Schedule: "*/1 * * * *"}
	now := time.Date(2026, 2, 27, 12, 34, 0, 0, time.UTC)
	result := CheckTrigger(a, now, neverRan, nil, nil)
	if !result.Due {
		t.Errorf("Due = false, want true for */1 schedule; reason=%q", result.Reason)
	}
}

func TestCheckTriggerCronNotMatched(t *testing.T) {
	a := Order{Name: "cleanup", Trigger: "cron", Schedule: "0 3 * * *"}
	// 12:00 UTC — should not match.
	now := time.Date(2026, 2, 27, 12, 0, 0, 0, time.UTC)
	result := CheckTrigger(a, now, neverRan, nil, nil)
	if result.Due {
		t.Errorf("Due = true, want false (schedule doesn't match 12:00)")
	}
}

func TestCheckTriggerCronCatchesUpMissedBoundary(t *testing.T) {
	// Regression (gastown td-4kziysy): a scheduled occurrence elapsed since
	// lastRun, but the controller evaluates at an off-schedule minute (its
	// eval cadence did not land in the exact matching minute). Cron must CATCH
	// UP and fire, the way cooldown's elapsed>=interval does. Unpatched
	// checkCron returns "schedule not matched" here and silently drops the
	// slot — which is why a "0 */4 * * *" order missed every boundary for days.
	a := Order{Name: "stale-db", Trigger: "cron", Schedule: "0 */4 * * *"}
	lastRun := time.Date(2026, 5, 29, 0, 0, 0, 0, time.UTC) // fired at the 00:00 boundary
	now := time.Date(2026, 5, 29, 4, 1, 0, 0, time.UTC)     // 04:00 boundary passed; eval at 04:01 (off-minute)
	lastRunFn := func(_ string) (time.Time, error) { return lastRun, nil }
	result := CheckTrigger(a, now, lastRunFn, nil, nil)
	if !result.Due {
		t.Errorf("Due = false, want true (catch up the missed 04:00 occurrence); reason=%q", result.Reason)
	}
}

func TestCheckTriggerCronAlreadyRunThisMinute(t *testing.T) {
	a := Order{Name: "cleanup", Trigger: "cron", Schedule: "0 3 * * *"}
	now := time.Date(2026, 2, 27, 3, 0, 30, 0, time.UTC)
	lastRun := time.Date(2026, 2, 27, 3, 0, 10, 0, time.UTC) // same minute
	lastRunFn := func(_ string) (time.Time, error) { return lastRun, nil }

	result := CheckTrigger(a, now, lastRunFn, nil, nil)
	if result.Due {
		t.Errorf("Due = true, want false (already run this minute)")
	}
}

func TestCheckTriggerCondition(t *testing.T) {
	a := Order{Name: "check", Trigger: "condition", Check: "true"}
	now := time.Date(2026, 2, 27, 12, 0, 0, 0, time.UTC)
	result := CheckTrigger(a, now, neverRan, nil, nil)
	if !result.Due {
		t.Errorf("Due = false, want true (exit 0)")
	}
}

func TestCheckTriggerConditionUsesOptions(t *testing.T) {
	dir := t.TempDir()
	if realDir, err := filepath.EvalSymlinks(dir); err == nil {
		dir = realDir
	}
	a := Order{
		Name:    "check",
		Trigger: "condition",
		Check:   `test "$GC_CITY_PATH" = "$EXPECT_CITY" && test "$(pwd -P)" = "$(cd "$EXPECT_CITY" && pwd -P)"`,
	}
	now := time.Date(2026, 2, 27, 12, 0, 0, 0, time.UTC)
	result := CheckTriggerWithOptions(a, now, neverRan, nil, nil, TriggerOptions{
		ConditionDir: dir,
		ConditionEnv: []string{
			"EXPECT_CITY=" + dir,
			"GC_CITY_PATH=" + dir,
		},
	})
	if !result.Due {
		t.Errorf("Due = false, want true with condition cwd/env: %s", result.Reason)
	}
}

func TestCheckTriggerConditionFails(t *testing.T) {
	a := Order{Name: "check", Trigger: "condition", Check: "false"}
	now := time.Date(2026, 2, 27, 12, 0, 0, 0, time.UTC)
	result := CheckTrigger(a, now, neverRan, nil, nil)
	if result.Due {
		t.Errorf("Due = true, want false (exit non-zero)")
	}
}

func TestCheckTriggerConditionKillsProcessGroupOnTimeout(t *testing.T) {
	processgrouptest.RequireRealProcessSignals(t)

	dir := t.TempDir()
	heartbeatPath := filepath.Join(dir, "heartbeat")
	childPIDPath := filepath.Join(dir, "child.pid")
	t.Cleanup(func() { processgrouptest.KillFromPIDFile(t, childPIDPath) })
	oldSignalGrace := conditionCheckSignalGrace
	conditionCheckSignalGrace = 100 * time.Millisecond
	t.Cleanup(func() { conditionCheckSignalGrace = oldSignalGrace })
	a := Order{
		Name:    "check",
		Trigger: "condition",
		Check:   fmt.Sprintf("sh -c 'printf \"%%s\\n\" \"$$\" > %q; trap \"\" TERM; while :; do printf . >> %q; sleep 0.05; done' & wait", childPIDPath, heartbeatPath),
	}
	now := time.Date(2026, 2, 27, 12, 0, 0, 0, time.UTC)
	result := CheckTriggerWithOptions(a, now, neverRan, nil, nil, TriggerOptions{
		ConditionDir:     dir,
		ConditionTimeout: 100 * time.Millisecond,
	})
	if result.Due {
		t.Fatalf("Due = true, want false after condition timeout")
	}
	if !strings.Contains(result.Reason, "timed out") {
		t.Fatalf("Reason = %q, want timeout", result.Reason)
	}

	size := processgrouptest.WaitForFileSize(t, heartbeatPath)
	processgrouptest.AssertFileSizeStable(t, heartbeatPath, size, 300*time.Millisecond)
}

func TestCheckTriggerConditionKillsProcessGroupAfterWaitDelay(t *testing.T) {
	processgrouptest.RequireRealProcessSignals(t)

	dir := t.TempDir()
	heartbeatPath := filepath.Join(dir, "heartbeat")
	childPIDPath := filepath.Join(dir, "child.pid")
	t.Cleanup(func() { processgrouptest.KillFromPIDFile(t, childPIDPath) })
	oldWaitDelay := conditionCheckPostCancelWaitDelay
	oldSignalGrace := conditionCheckSignalGrace
	conditionCheckPostCancelWaitDelay = 100 * time.Millisecond
	conditionCheckSignalGrace = 100 * time.Millisecond
	t.Cleanup(func() {
		conditionCheckPostCancelWaitDelay = oldWaitDelay
		conditionCheckSignalGrace = oldSignalGrace
	})
	a := Order{
		Name:    "check",
		Trigger: "condition",
		Check:   fmt.Sprintf("sh -c 'printf \"%%s\\n\" \"$$\" > %q; trap \"\" TERM; while :; do printf . >> %q; sleep 0.05; done' &", childPIDPath, heartbeatPath),
	}
	now := time.Date(2026, 2, 27, 12, 0, 0, 0, time.UTC)
	result := CheckTriggerWithOptions(a, now, neverRan, nil, nil, TriggerOptions{
		ConditionDir:     dir,
		ConditionTimeout: 10 * time.Second,
	})
	if result.Due {
		t.Fatalf("Due = true, want false after condition post-cancel wait delay")
	}
	if !strings.Contains(result.Reason, "post-cancel wait delay") {
		t.Fatalf("Reason = %q, want post-cancel wait delay", result.Reason)
	}

	size := processgrouptest.WaitForFileSize(t, heartbeatPath)
	processgrouptest.AssertFileSizeStable(t, heartbeatPath, size, 300*time.Millisecond)
}

func TestCronFieldMatches(t *testing.T) {
	tests := []struct {
		field string
		value int
		want  bool
	}{
		{"*", 5, true},
		{"5", 5, true},
		{"5", 3, false},
		{"1,3,5", 3, true},
		{"1,3,5", 2, false},
	}
	for _, tt := range tests {
		got := cronFieldMatches(tt.field, tt.value)
		if got != tt.want {
			t.Errorf("cronFieldMatches(%q, %d) = %v, want %v", tt.field, tt.value, got, tt.want)
		}
	}
}

// newEventsProvider creates a FileRecorder-backed Provider with events for tests.
func newEventsProvider(t *testing.T, evts []events.Event) events.Provider {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer
	rec, err := events.NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evts {
		rec.Record(e)
	}
	t.Cleanup(func() { rec.Close() }) //nolint:errcheck // test cleanup
	return rec
}

func TestCheckTriggerEventDue(t *testing.T) {
	ep := newEventsProvider(t, []events.Event{
		{Type: "bead.closed"},
		{Type: "bead.created"},
		{Type: "bead.closed"},
	})
	a := Order{Name: "convoy-check", Trigger: "event", On: "bead.closed"}
	// nil cursorFn → cursor=0 → all events considered.
	result := CheckTrigger(a, time.Time{}, neverRan, ep, nil)
	if !result.Due {
		t.Errorf("Due = false, want true; reason: %s", result.Reason)
	}
	if result.Reason != "event: 2 bead.closed event(s)" {
		t.Errorf("Reason = %q, want %q", result.Reason, "event: 2 bead.closed event(s)")
	}
}

func TestCheckTriggerEventWithCursor(t *testing.T) {
	ep := newEventsProvider(t, []events.Event{
		{Type: "bead.closed"},
		{Type: "bead.created"},
		{Type: "bead.closed"},
	})
	a := Order{Name: "convoy-check", Trigger: "event", On: "bead.closed"}
	// Cursor at seq 2 → only seq 3 matches.
	cursorFn := func(_ string) uint64 { return 2 }
	result := CheckTrigger(a, time.Time{}, neverRan, ep, cursorFn)
	if !result.Due {
		t.Errorf("Due = false, want true; reason: %s", result.Reason)
	}
	if result.Reason != "event: 1 bead.closed event(s)" {
		t.Errorf("Reason = %q, want %q", result.Reason, "event: 1 bead.closed event(s)")
	}
}

func TestCheckTriggerEventCursorPastAll(t *testing.T) {
	ep := newEventsProvider(t, []events.Event{
		{Type: "bead.closed"},
		{Type: "bead.closed"},
	})
	a := Order{Name: "convoy-check", Trigger: "event", On: "bead.closed"}
	// Cursor past all events → not due.
	cursorFn := func(_ string) uint64 { return 5 }
	result := CheckTrigger(a, time.Time{}, neverRan, ep, cursorFn)
	if result.Due {
		t.Errorf("Due = true, want false (cursor past all events)")
	}
}

func TestCheckTriggerEventNotDue(t *testing.T) {
	ep := newEventsProvider(t, []events.Event{
		{Type: "bead.created"},
		{Type: "bead.updated"},
	})
	a := Order{Name: "convoy-check", Trigger: "event", On: "bead.closed"}
	result := CheckTrigger(a, time.Time{}, neverRan, ep, nil)
	if result.Due {
		t.Errorf("Due = true, want false (no matching events)")
	}
}

func TestCheckTriggerEventNoEventsProvider(t *testing.T) {
	a := Order{Name: "convoy-check", Trigger: "event", On: "bead.closed"}
	result := CheckTrigger(a, time.Time{}, neverRan, nil, nil)
	if result.Due {
		t.Errorf("Due = true, want false (nil provider)")
	}
}

func TestCheckTriggerCooldownRigScoped(t *testing.T) {
	// Rig order should query with scoped name; city order with plain name.
	now := time.Date(2026, 2, 27, 12, 0, 0, 0, time.UTC)

	queriedNames := []string{}
	lastRunFn := func(name string) (time.Time, error) {
		queriedNames = append(queriedNames, name)
		return time.Time{}, nil
	}

	// Rig-scoped order.
	rigA := Order{Name: "dolt-health", Rig: "demo-repo", Trigger: "cooldown", Interval: "1h"}
	CheckTrigger(rigA, now, lastRunFn, nil, nil)

	// City-level order.
	cityA := Order{Name: "dolt-health", Trigger: "cooldown", Interval: "1h"}
	CheckTrigger(cityA, now, lastRunFn, nil, nil)

	if len(queriedNames) != 2 {
		t.Fatalf("expected 2 queries, got %d", len(queriedNames))
	}
	if queriedNames[0] != "dolt-health:rig:demo-repo" {
		t.Errorf("rig query = %q, want %q", queriedNames[0], "dolt-health:rig:demo-repo")
	}
	if queriedNames[1] != "dolt-health" {
		t.Errorf("city query = %q, want %q", queriedNames[1], "dolt-health")
	}
}

func TestCheckTriggerCronRigScoped(t *testing.T) {
	// Rig order cron trigger queries scoped name.
	now := time.Date(2026, 2, 27, 3, 0, 0, 0, time.UTC) // matches "0 3 * * *"

	var queriedName string
	lastRunFn := func(name string) (time.Time, error) {
		queriedName = name
		return time.Time{}, nil
	}

	a := Order{Name: "cleanup", Rig: "my-rig", Trigger: "cron", Schedule: "0 3 * * *"}
	CheckTrigger(a, now, lastRunFn, nil, nil)

	if queriedName != "cleanup:rig:my-rig" {
		t.Errorf("cron query = %q, want %q", queriedName, "cleanup:rig:my-rig")
	}
}

func TestCheckTriggerEventOrderTrackingBeadsFiltered(t *testing.T) {
	// Regression: event orders must not self-fire on bead lifecycle events emitted
	// by order-tracking beads (controller bookkeeping). This was the root cause of
	// the ~80 events/min feedback loop after ce32c6bf6 switched tracking beads from
	// Ephemeral to NoHistory, making their lifecycle events visible to the cache.
	trackingPayload := mustMarshalLabels(t, []string{"order-run:nudge-on-route", "order-tracking"})
	regularPayload := mustMarshalLabels(t, []string{"work:some-bead"})

	ep := newEventsProvider(t, []events.Event{
		{Type: "bead.updated", Payload: trackingPayload}, // order-tracking — excluded
		{Type: "bead.updated", Payload: regularPayload},  // real work bead — counted
		{Type: "bead.updated", Payload: trackingPayload}, // order-tracking — excluded
	})
	a := Order{Name: "nudge-on-route", Trigger: "event", On: "bead.updated"}
	result := CheckTrigger(a, time.Time{}, neverRan, ep, nil)
	if !result.Due {
		t.Errorf("Due = false, want true (one non-tracking bead.updated exists); reason: %s", result.Reason)
	}
	if result.Reason != "event: 1 bead.updated event(s)" {
		t.Errorf("Reason = %q, want %q", result.Reason, "event: 1 bead.updated event(s)")
	}
}

func TestCheckTriggerEventAllOrderTrackingFiltered(t *testing.T) {
	// When ALL matched events come from order-tracking beads the order is not due.
	trackingPayload := mustMarshalLabels(t, []string{"order-run:nudge-on-route", "order-tracking"})

	ep := newEventsProvider(t, []events.Event{
		{Type: "bead.updated", Payload: trackingPayload},
		{Type: "bead.closed", Payload: trackingPayload},
	})
	a := Order{Name: "nudge-on-route", Trigger: "event", On: "bead.updated"}
	result := CheckTrigger(a, time.Time{}, neverRan, ep, nil)
	if result.Due {
		t.Errorf("Due = true, want false (all events from order-tracking beads); reason: %s", result.Reason)
	}
}

func TestCheckTriggerEventNoPayloadNotFiltered(t *testing.T) {
	// Events with no payload (legacy or non-bead events) must pass through —
	// absence of a label is not the same as having the order-tracking label.
	ep := newEventsProvider(t, []events.Event{
		{Type: "bead.closed"}, // no payload
	})
	a := Order{Name: "convoy-check", Trigger: "event", On: "bead.closed"}
	result := CheckTrigger(a, time.Time{}, neverRan, ep, nil)
	if !result.Due {
		t.Errorf("Due = false, want true (no-payload events must not be filtered); reason: %s", result.Reason)
	}
}

func mustMarshalLabels(t *testing.T, labels []string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(struct {
		Labels []string `json:"labels"`
	}{Labels: labels})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCheckTriggerEventRigScoped(t *testing.T) {
	ep := newEventsProvider(t, []events.Event{
		{Type: "bead.closed"},
	})

	var queriedName string
	cursorFn := func(name string) uint64 {
		queriedName = name
		return 0
	}

	a := Order{Name: "convoy-check", Rig: "my-rig", Trigger: "event", On: "bead.closed"}
	CheckTrigger(a, time.Time{}, neverRan, ep, cursorFn)

	if queriedName != "convoy-check:rig:my-rig" {
		t.Errorf("event cursor query = %q, want %q", queriedName, "convoy-check:rig:my-rig")
	}
}

func TestMaxSeqFromLabels(t *testing.T) {
	tests := []struct {
		name   string
		labels [][]string
		want   uint64
	}{
		{
			name:   "single wisp",
			labels: [][]string{{"order:convoy-check", "seq:42"}},
			want:   42,
		},
		{
			name:   "multiple wisps pick max",
			labels: [][]string{{"order:convoy-check", "seq:10"}, {"order:convoy-check", "seq:99"}},
			want:   99,
		},
		{
			name:   "mixed labels",
			labels: [][]string{{"pool:dog", "seq:5", "order:convoy-check"}},
			want:   5,
		},
		{
			name:   "no seq labels",
			labels: [][]string{{"order:convoy-check"}},
			want:   0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MaxSeqFromLabels(tt.labels)
			if got != tt.want {
				t.Errorf("MaxSeqFromLabels = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestMaxSeqFromLabelsEmpty(t *testing.T) {
	tests := []struct {
		name   string
		labels [][]string
	}{
		{"nil", nil},
		{"empty", [][]string{}},
		{"no labels", [][]string{{}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MaxSeqFromLabels(tt.labels)
			if got != 0 {
				t.Errorf("MaxSeqFromLabels = %d, want 0", got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Cron time-zone handling (fix/cron-catchup-single-location; follow-up to the
// #2721 catch-up scan).
//
// checkCron used to mix two time domains: the live match (a) evaluated cron
// fields in `now`'s location, while the catch-up scan (b) walked minutes in
// the last-run bead's location — which the doltlite store ALWAYS returns
// UTC-located (parseTimeString). On a non-UTC box a zone-anchored order fired
// at the UTC reading of its slot ("30 19 * * *" fired at 19:30Z == 15:30 ET)
// and then AGAIN at the real local slot: two fires per day. These tests pin
// the fix: one explicit location (order tz → city default → process-local),
// with both `now` and lastRun normalized into it. All orders here set tz so
// the tests are independent of the test box's TZ.
// ---------------------------------------------------------------------------

func etCronOrder(t *testing.T, schedule string) (Order, *time.Location) {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load America/New_York: %v", err)
	}
	return Order{Name: "et-order", Trigger: "cron", Schedule: schedule, TZ: "America/New_York"}, loc
}

func fixedLastRun(last time.Time) LastRunFunc {
	return func(string) (time.Time, error) { return last, nil }
}

// Regression: PM early fire at the UTC reading. Schedule "30 19 * * *" means
// 19:30 ET (23:30Z during EDT). Last correct fire Jul 6 23:30Z (store-shaped:
// UTC-located). Tick at Jul 7 19:30:30Z == 15:30:30 ET must NOT fire.
// Pre-fix: due=true "cron: caught up missed occurrence" — the catch-up scan
// matched hour 19 on the UTC wall clock.
func TestCheckTriggerCronCatchupDoesNotFireAtUTCReadingPM(t *testing.T) {
	a, loc := etCronOrder(t, "30 19 * * *")
	last := time.Date(2026, 7, 6, 23, 30, 0, 0, time.UTC) // == Jul 6 19:30 ET
	now := time.Date(2026, 7, 7, 15, 30, 30, 0, loc)      // == 19:30:30Z
	res := checkCron(a, now, fixedLastRun(last))
	if res.Due {
		t.Errorf("due=true reason=%q at %s, want false (next fire is 19:30 ET / 23:30Z)",
			res.Reason, now.UTC().Format(time.RFC3339))
	}
}

// Regression: AM early fire — the exact live signature (dispatch at
// 07:00:19Z == 03:00:19 ET for a "0 7 * * *" order meant as 07:00 ET).
func TestCheckTriggerCronCatchupDoesNotFireAtUTCReadingAM(t *testing.T) {
	a, loc := etCronOrder(t, "0 7 * * *")
	last := time.Date(2026, 7, 6, 11, 0, 0, 0, time.UTC) // == Jul 6 07:00 ET
	now := time.Date(2026, 7, 7, 3, 0, 19, 0, loc)       // == 07:00:19Z
	res := checkCron(a, now, fixedLastRun(last))
	if res.Due {
		t.Errorf("due=true reason=%q at %s, want false (next fire is 07:00 ET / 11:00Z)",
			res.Reason, now.UTC().Format(time.RFC3339))
	}
}

// Control: at the real zone slot the order fires.
func TestCheckTriggerCronFiresAtRealZoneSlot(t *testing.T) {
	a, loc := etCronOrder(t, "0 7 * * *")
	last := time.Date(2026, 7, 6, 11, 0, 0, 0, time.UTC)
	now := time.Date(2026, 7, 7, 7, 0, 30, 0, loc) // 07:00:30 ET == 11:00:30Z
	res := checkCron(a, now, fixedLastRun(last))
	if !res.Due {
		t.Errorf("due=false reason=%q, want true at the real 07:00 ET slot", res.Reason)
	}
}

// The order's tz — not the caller's location and not time.Local — decides
// the wall clock: with tz=America/New_York and UTC-located nows, 11:00:19Z
// (07:00 ET) fires and 07:00:19Z (03:00 ET) does not.
func TestCheckTriggerCronSpecTZIndependentOfCallerLocation(t *testing.T) {
	a, _ := etCronOrder(t, "0 7 * * *")
	last := time.Date(2026, 7, 6, 11, 0, 0, 0, time.UTC)

	atSlot := time.Date(2026, 7, 7, 11, 0, 19, 0, time.UTC) // == 07:00:19 ET
	if res := checkCron(a, atSlot, fixedLastRun(last)); !res.Due {
		t.Errorf("at 11:00:19Z (07:00 ET): due=false reason=%q, want true", res.Reason)
	}

	offSlot := time.Date(2026, 7, 7, 7, 0, 19, 0, time.UTC) // == 03:00:19 ET
	if res := checkCron(a, offSlot, fixedLastRun(last)); res.Due {
		t.Errorf("at 07:00:19Z (03:00 ET): due=true reason=%q, want false", res.Reason)
	}
}

// A full simulated day of 30s ticks yields exactly one fire, at the zone
// slot. Pre-fix this produced two fires: 15:30 ET (the UTC reading, via
// catch-up) and 19:30 ET (the live match). The store round-trips lastRun
// UTC-located, as doltlite does; the caller's tick location must not matter.
func TestCheckTriggerCronExactlyOneFirePerSlot(t *testing.T) {
	_, et := etCronOrder(t, "30 19 * * *")
	for name, callerLoc := range map[string]*time.Location{"utc-caller": time.UTC, "et-caller": et} {
		t.Run(name, func(t *testing.T) {
			a, _ := etCronOrder(t, "30 19 * * *")
			last := time.Date(2026, 7, 6, 23, 30, 5, 0, time.UTC) // yesterday's correct fire
			lastRunFn := func(string) (time.Time, error) { return last, nil }

			start := time.Date(2026, 7, 7, 0, 0, 0, 0, et).In(callerLoc)
			var fires []string
			for tick := start; tick.Before(start.Add(24 * time.Hour)); tick = tick.Add(30 * time.Second) {
				if res := checkCron(a, tick, lastRunFn); res.Due {
					fires = append(fires, tick.In(et).Format(time.RFC3339)+" ("+res.Reason+")")
					last = tick.UTC() // store round-trip: doltlite returns UTC-located
				}
			}
			if len(fires) != 1 || !strings.HasPrefix(fires[0], "2026-07-07T19:30:00-04:00") {
				t.Errorf("fires = %v, want exactly one at 2026-07-07T19:30 ET", fires)
			}
		})
	}
}

// Catch-up still works in-zone across a multi-day gap: a missed occurrence
// between lastRun and now fires with the catch-up reason.
func TestCheckTriggerCronCatchupAcrossMultiDayGapInZone(t *testing.T) {
	a, loc := etCronOrder(t, "0 7 * * *")
	last := time.Date(2026, 7, 4, 11, 0, 0, 0, time.UTC) // Jul 4 07:00 ET
	now := time.Date(2026, 7, 7, 3, 0, 0, 0, loc)        // off-slot eval, two slots missed
	res := checkCron(a, now, fixedLastRun(last))
	if !res.Due || res.Reason != "cron: caught up missed occurrence" {
		t.Errorf("due=%v reason=%q, want catch-up fire for the missed Jul 5/6 07:00 ET slots", res.Due, res.Reason)
	}
}

// DST fall-back (US 2026-11-01: 02:00 EDT → 01:00 EST): the 01:xx hour
// repeats. Policy: at most one fire per wall-clock slot — the repeated
// reading is deduped against lastRun by wall-clock date+HH:MM.
func TestCheckTriggerCronDSTFallBackFiresOncePerWallClockSlot(t *testing.T) {
	t.Run("live repeat deduped", func(t *testing.T) {
		a, loc := etCronOrder(t, "30 1 * * *")
		// Fired at 01:30 EDT (05:30Z); store hands it back UTC-located.
		last := time.Date(2026, 11, 1, 5, 30, 10, 0, time.UTC)
		now := time.Date(2026, 11, 1, 6, 30, 20, 0, time.UTC).In(loc) // second 01:30 (EST)
		res := checkCron(a, now, fixedLastRun(last))
		if res.Due {
			t.Errorf("due=true reason=%q, want false (01:30 already fired this wall-clock day)", res.Reason)
		}
	})
	t.Run("catch-up repeat deduped", func(t *testing.T) {
		a, loc := etCronOrder(t, "30 1 * * *")
		last := time.Date(2026, 11, 1, 5, 30, 10, 0, time.UTC)       // 01:30:10 EDT
		now := time.Date(2026, 11, 1, 6, 45, 0, 0, time.UTC).In(loc) // 01:45 EST; scan crosses 01:30 EST
		res := checkCron(a, now, fixedLastRun(last))
		if res.Due {
			t.Errorf("due=true reason=%q, want false (catch-up must not re-fire the repeated 01:30)", res.Reason)
		}
	})
	t.Run("one fire across the transition night", func(t *testing.T) {
		a, loc := etCronOrder(t, "30 1 * * *")
		last := time.Date(2026, 10, 31, 5, 30, 0, 0, time.UTC) // yesterday's 01:30 EDT
		lastRunFn := func(string) (time.Time, error) { return last, nil }
		start := time.Date(2026, 11, 1, 0, 0, 0, 0, loc) // 00:00 EDT
		var fires []string
		for tick := start; tick.Before(start.Add(5 * time.Hour)); tick = tick.Add(30 * time.Second) {
			if res := checkCron(a, tick, lastRunFn); res.Due {
				fires = append(fires, tick.Format(time.RFC3339)+" ("+res.Reason+")")
				last = tick.UTC()
			}
		}
		if len(fires) != 1 || !strings.HasPrefix(fires[0], "2026-11-01T01:30:00-04:00") {
			t.Errorf("fires = %v, want exactly one at the first (EDT) 01:30", fires)
		}
	})
}

// DST spring-forward (US 2027-03-14: 02:00 EST → 03:00 EDT): the 02:xx hour
// does not exist. Policy: a schedule inside the gap fires once at the first
// real minute after the jump (03:00), via the catch-up scan's gap detection.
func TestCheckTriggerCronDSTSpringForwardGapFiresAtNextRealMinute(t *testing.T) {
	t.Run("gap schedule fires at 03:00", func(t *testing.T) {
		a, loc := etCronOrder(t, "30 2 * * *")
		last := time.Date(2027, 3, 13, 7, 30, 0, 0, time.UTC) // yesterday's 02:30 EST
		now := time.Date(2027, 3, 14, 3, 0, 10, 0, loc)       // first real minute after the gap
		res := checkCron(a, now, fixedLastRun(last))
		if !res.Due || res.Reason != "cron: caught up occurrence skipped by DST spring-forward" {
			t.Errorf("due=%v reason=%q, want spring-forward gap fire at 03:00 EDT", res.Due, res.Reason)
		}
	})
	t.Run("no second fire after the gap fire", func(t *testing.T) {
		a, loc := etCronOrder(t, "30 2 * * *")
		last := time.Date(2027, 3, 14, 7, 0, 10, 0, time.UTC) // the 03:00:10 EDT gap fire, store-shaped
		now := time.Date(2027, 3, 14, 3, 5, 0, 0, loc)
		res := checkCron(a, now, fixedLastRun(last))
		if res.Due {
			t.Errorf("due=true reason=%q, want false (gap already caught up)", res.Reason)
		}
	})
	t.Run("one fire across the transition night", func(t *testing.T) {
		a, loc := etCronOrder(t, "30 2 * * *")
		last := time.Date(2027, 3, 13, 7, 30, 0, 0, time.UTC)
		lastRunFn := func(string) (time.Time, error) { return last, nil }
		start := time.Date(2027, 3, 14, 0, 0, 0, 0, loc)
		var fires []string
		for tick := start; tick.Before(start.Add(5 * time.Hour)); tick = tick.Add(30 * time.Second) {
			if res := checkCron(a, tick, lastRunFn); res.Due {
				fires = append(fires, tick.Format(time.RFC3339)+" ("+res.Reason+")")
				last = tick.UTC()
			}
		}
		if len(fires) != 1 || !strings.HasPrefix(fires[0], "2027-03-14T03:00:00-04:00") {
			t.Errorf("fires = %v, want exactly one at 03:00 EDT (the minute after the skipped 02:30)", fires)
		}
	})
}

// A bad tz never silently falls back: checkCron refuses to evaluate.
// (Order load rejects it earlier — see TestValidateCronBadTZ.)
func TestCheckTriggerCronBadTZFailsClosed(t *testing.T) {
	a := Order{Name: "bad-tz", Trigger: "cron", Schedule: "0 7 * * *", TZ: "America/New_Yrok"}
	now := time.Date(2026, 7, 7, 11, 0, 19, 0, time.UTC)
	res := CheckTrigger(a, now, neverRan, nil, nil)
	if res.Due || !strings.Contains(res.Reason, "bad tz") {
		t.Errorf("due=%v reason=%q, want fail-closed with a bad-tz reason", res.Due, res.Reason)
	}
}
