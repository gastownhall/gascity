package orders

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
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

func TestCheckTriggerCronNotMatched(t *testing.T) {
	a := Order{Name: "cleanup", Trigger: "cron", Schedule: "0 3 * * *"}
	// 12:00 UTC — should not match.
	now := time.Date(2026, 2, 27, 12, 0, 0, 0, time.UTC)
	result := CheckTrigger(a, now, neverRan, nil, nil)
	if result.Due {
		t.Errorf("Due = true, want false (schedule doesn't match 12:00)")
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
