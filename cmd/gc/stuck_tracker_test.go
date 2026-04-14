package main

import (
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

// doStuckTracker constructs a memoryStuckTracker for tests with explicit
// daemon config knobs. Mirrors the do*() convention used by idle/crash
// tracker tests for readable call-sites.
func doStuckTracker(t *testing.T, patterns []string) stuckTracker {
	t.Helper()
	peekLines := 50
	d := config.DaemonConfig{
		StuckSweep:         true,
		StuckWispThreshold: (10 * time.Minute).String(),
		StuckErrorPatterns: patterns,
		StuckPeekLines:     &peekLines,
		StuckWarrantLabel:  "pool:dog",
	}
	tr, err := newStuckTracker(d)
	if err != nil {
		t.Fatalf("newStuckTracker: %v", err)
	}
	return tr
}

func TestStuckTracker_DisabledReturnsNoop(t *testing.T) {
	// StuckSweep=false → noopStuckTracker, always reports not-stuck.
	tr, err := newStuckTracker(config.DaemonConfig{})
	if err != nil {
		t.Fatalf("newStuckTracker: %v", err)
	}
	if _, ok := tr.(noopStuckTracker); !ok {
		t.Fatalf("expected noopStuckTracker when disabled, got %T", tr)
	}
	now := time.Now()
	stuck, _, _ := tr.checkStuck("s1", "ERROR: boom", now.Add(-time.Hour), now.Add(-time.Hour), now)
	if stuck {
		t.Fatal("noop tracker must never report stuck")
	}
}

func TestStuckTracker_EmptyPatternsKeepsTracker(t *testing.T) {
	// Post-M3: StuckSweep=true with no patterns is valid — regex axis
	// is disabled but progress-mismatch axis remains live, so we still
	// construct a memoryStuckTracker with an empty patterns slice.
	tr, err := newStuckTracker(config.DaemonConfig{
		StuckSweep:        true,
		StuckWarrantLabel: "pool:dog",
	})
	if err != nil {
		t.Fatalf("newStuckTracker: %v", err)
	}
	m, ok := tr.(*memoryStuckTracker)
	if !ok {
		t.Fatalf("expected *memoryStuckTracker when patterns empty, got %T", tr)
	}
	if len(m.patterns) != 0 {
		t.Fatalf("expected zero compiled patterns, got %d", len(m.patterns))
	}
}

func TestStuckTracker_EmptyPatternsProgressMismatchFires(t *testing.T) {
	// Post-M3: with no patterns, the progress-mismatch axis still classifies
	// a stale wisp + stale activity as stuck.
	tr, err := newStuckTracker(config.DaemonConfig{
		StuckSweep:         true,
		StuckWispThreshold: (10 * time.Minute).String(),
		StuckWarrantLabel:  "pool:dog",
	})
	if err != nil {
		t.Fatalf("newStuckTracker: %v", err)
	}
	now := time.Now()
	wispOpened := now.Add(-30 * time.Minute)
	lastActivity := wispOpened.Add(-30 * time.Minute) // older than wisp, well past threshold
	stuck, reason, matched := tr.checkStuck("s1", "quiet pane", wispOpened, lastActivity, now)
	if !stuck {
		t.Fatalf("expected stuck via progress-mismatch with empty patterns; reason=%q", reason)
	}
	if matched != "" {
		t.Fatalf("expected empty matched pattern, got %q", matched)
	}
	if !strings.Contains(reason, "no progress") {
		t.Fatalf("reason should mention progress: %q", reason)
	}
}

func TestStuckTracker_InvalidRegexFailsFast(t *testing.T) {
	_, err := newStuckTracker(config.DaemonConfig{
		StuckSweep:         true,
		StuckErrorPatterns: []string{"fine", "(unclosed"},
		StuckWarrantLabel:  "pool:dog",
	})
	if err == nil {
		t.Fatal("expected error for invalid regex, got nil")
	}
	if !strings.Contains(err.Error(), "stuck_error_patterns[1]") {
		t.Fatalf("error should identify bad index: %v", err)
	}
}

func TestStuckTracker_StalePlusPatternDetected(t *testing.T) {
	tr := doStuckTracker(t, []string{`(?i)rate limit`})
	now := time.Now()
	// Wisp opened 30m ago (stale); activity recent so no progress-mismatch.
	stuck, reason, matched := tr.checkStuck(
		"s1",
		"hello\nrate limit exceeded\n",
		now.Add(-30*time.Minute),
		now.Add(-30*time.Second),
		now,
	)
	if !stuck {
		t.Fatalf("expected stuck; reason=%q", reason)
	}
	if matched == "" {
		t.Fatal("expected matched pattern to be populated")
	}
	if !strings.Contains(reason, "pattern") {
		t.Fatalf("reason should mention pattern: %q", reason)
	}
}

func TestStuckTracker_StalePlusProgressMismatchDetected(t *testing.T) {
	// No pattern match but last-activity is older than the wisp UpdatedAt
	// by more than the threshold → stuck on the progress-mismatch axis.
	tr := doStuckTracker(t, []string{`never-matches-xyz`})
	now := time.Now()
	wispOpened := now.Add(-30 * time.Minute)
	lastActivity := wispOpened.Add(-30 * time.Minute) // older than wisp, > threshold old overall
	stuck, reason, matched := tr.checkStuck("s1", "quiet pane", wispOpened, lastActivity, now)
	if !stuck {
		t.Fatalf("expected stuck via progress-mismatch; reason=%q", reason)
	}
	if matched != "" {
		t.Fatalf("no pattern should have matched, got %q", matched)
	}
	if !strings.Contains(reason, "no progress") {
		t.Fatalf("reason should mention progress: %q", reason)
	}
}

func TestStuckTracker_FreshWispNotDetected(t *testing.T) {
	tr := doStuckTracker(t, []string{`(?i)rate limit`})
	now := time.Now()
	// Wisp opened 2 minutes ago — fresh — even with an error pattern present.
	stuck, _, _ := tr.checkStuck(
		"s1",
		"rate limit exceeded",
		now.Add(-2*time.Minute),
		now.Add(-1*time.Minute),
		now,
	)
	if stuck {
		t.Fatal("fresh wisp must not be flagged even on pattern match")
	}
}

func TestStuckTracker_StaleButNoPatternAndActivityAlignedNotDetected(t *testing.T) {
	tr := doStuckTracker(t, []string{`never-matches-xyz`})
	now := time.Now()
	wispOpened := now.Add(-30 * time.Minute)
	// Activity AFTER wisp open → no progress-mismatch.
	lastActivity := now.Add(-30 * time.Second)
	stuck, _, _ := tr.checkStuck("s1", "still working", wispOpened, lastActivity, now)
	if stuck {
		t.Fatal("stale wisp with aligned activity and no pattern must not be flagged")
	}
}

func TestStuckTracker_ZeroWispUpdatedAtFailOpen(t *testing.T) {
	tr := doStuckTracker(t, []string{`(?i)error`})
	now := time.Now()
	stuck, _, _ := tr.checkStuck("s1", "error error", time.Time{}, now.Add(-time.Hour), now)
	if stuck {
		t.Fatal("zero wispUpdatedAt must fail-open (not stuck)")
	}
}

func TestStuckTracker_BoundaryEqualsThresholdNotDetected(t *testing.T) {
	// E16: strict > against threshold.
	tr := doStuckTracker(t, []string{`(?i)error`})
	now := time.Now()
	wispOpened := now.Add(-10 * time.Minute) // exactly at threshold
	stuck, _, _ := tr.checkStuck("s1", "error", wispOpened, now.Add(-time.Second), now)
	if stuck {
		t.Fatal("wisp age exactly at threshold must not be flagged (strict >)")
	}
}

func TestStuckTracker_MultiplePatternsDeterministicJoin(t *testing.T) {
	// E12: multiple matches → sorted comma-joined matched string.
	tr := doStuckTracker(t, []string{`zeta`, `alpha`, `beta`})
	now := time.Now()
	stuck, _, matched := tr.checkStuck(
		"s1",
		"alpha beta zeta together",
		now.Add(-20*time.Minute),
		now.Add(-time.Second),
		now,
	)
	if !stuck {
		t.Fatal("expected stuck")
	}
	if matched != "alpha,beta,zeta" {
		t.Fatalf("matched should be sorted-joined, got %q", matched)
	}
}

func TestStuckTracker_EmptyPaneAndNoMismatchNotDetected(t *testing.T) {
	tr := doStuckTracker(t, []string{`(?i)error`})
	now := time.Now()
	wispOpened := now.Add(-30 * time.Minute)
	// Empty pane, activity aligned with wisp (no mismatch).
	stuck, _, _ := tr.checkStuck("s1", "", wispOpened, now.Add(-time.Second), now)
	if stuck {
		t.Fatal("empty pane with no progress-mismatch must not be flagged")
	}
}

func TestStuckTracker_PeekLinesAndLabelAccessors(t *testing.T) {
	d := config.DaemonConfig{
		StuckSweep:         true,
		StuckErrorPatterns: []string{`x`},
		StuckPeekLines:     nil, // nil → default
	}
	tr, err := newStuckTracker(d)
	if err != nil {
		t.Fatalf("newStuckTracker: %v", err)
	}
	m, ok := tr.(*memoryStuckTracker)
	if !ok {
		t.Fatalf("expected *memoryStuckTracker, got %T", tr)
	}
	if m.peekLinesOrDefault() != config.DefaultStuckPeekLines {
		t.Fatalf("peekLines default not applied: %d", m.peekLinesOrDefault())
	}
	if m.warrantLabelOrDefault() != config.DefaultStuckWarrantLabel {
		t.Fatalf("warrantLabel default not applied: %q", m.warrantLabelOrDefault())
	}
	if m.wispThresholdDuration() <= 0 {
		t.Fatalf("wispThreshold default not applied: %v", m.wispThresholdDuration())
	}
}

func TestStuckSweepEnabled_RequiresAllPreconditions(t *testing.T) {
	cases := []struct {
		name string
		d    config.DaemonConfig
		want bool
	}{
		{"all unset", config.DaemonConfig{}, false},
		{"flag off", config.DaemonConfig{StuckErrorPatterns: []string{"x"}}, false},
		{"flag+label (no patterns)", config.DaemonConfig{StuckSweep: true, StuckWarrantLabel: "pool:dog"}, true},
		{"flag+pattern (default label applies)", config.DaemonConfig{StuckSweep: true, StuckErrorPatterns: []string{"x"}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.d.StuckSweepEnabled(); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
