package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

// stuckTracker is a pure predicate that classifies a session as stuck or
// not-stuck based on cognition-independent signals supplied by the caller:
// recent pane output, the session's in-progress wisp updated-at time, the
// session's last I/O-activity time, and the current wall-clock time.
//
// Following the nil-guard convention used by idleTracker, crashTracker, and
// wispGC, the tracker does no I/O and reads no global clock. The CityRuntime
// wrapper is responsible for collecting inputs (via runtime.Provider.Peek,
// runtime.Provider.GetLastActivity, and a bd list shellout for wisp
// freshness) and for acting on a positive result (filing a warrant bead,
// emitting the agent.stuck event).
//
// The pure-predicate shape matches R10 of the design brief and enables
// deterministic table-driven tests.
type stuckTracker interface {
	// checkStuck returns (true, reason) if the session should be flagged as
	// stuck, or (false, "") otherwise. The reason string is a short
	// human-readable explanation suitable for embedding in a warrant's
	// metadata and the agent.stuck event message.
	//
	// matchedPattern is the regex source string of the first pattern that
	// matched paneOutput, or a sorted comma-joined list when multiple
	// patterns matched. Empty when the positive result derives from the
	// progress-mismatch axis alone.
	checkStuck(
		session string,
		paneOutput string,
		wispUpdatedAt time.Time,
		lastActivity time.Time,
		now time.Time,
	) (stuck bool, reason string, matchedPattern string)
}

// noopStuckTracker is returned when the sweep is disabled. It always reports
// not-stuck so callers can use it without nil-guarding.
type noopStuckTracker struct{}

func (noopStuckTracker) checkStuck(string, string, time.Time, time.Time, time.Time) (bool, string, string) {
	return false, "", ""
}

// memoryStuckTracker is the production implementation. Compiled regexes are
// immutable after construction (R13 / round-2 disagreement resolved): the
// slice is never mutated; reload builds a fresh tracker.
type memoryStuckTracker struct {
	wispThreshold time.Duration
	patterns      []*regexp.Regexp
	peekLines     int
	warrantLabel  string
}

// wispThresholdDuration returns the configured staleness threshold. Exposed
// so the CityRuntime wrapper can decide how far back to Peek or adjust its
// own behavior without re-reading config.
func (m *memoryStuckTracker) wispThresholdDuration() time.Duration {
	return m.wispThreshold
}

// peekLinesOrDefault returns the configured peek-lines setting.
func (m *memoryStuckTracker) peekLinesOrDefault() int {
	return m.peekLines
}

// warrantLabelOrDefault returns the configured warrant label.
func (m *memoryStuckTracker) warrantLabelOrDefault() string {
	return m.warrantLabel
}

// newStuckTracker constructs the tracker from daemon config. Returns a
// noopStuckTracker when the sweep is disabled or misconfigured. Returns an
// error when any pattern fails to compile; callers should treat this as a
// fail-fast startup error (E3).
func newStuckTracker(d config.DaemonConfig) (stuckTracker, error) {
	if !d.StuckSweepEnabled() {
		return noopStuckTracker{}, nil
	}
	patterns := make([]*regexp.Regexp, 0, len(d.StuckErrorPatterns))
	for i, src := range d.StuckErrorPatterns {
		re, err := regexp.Compile(src)
		if err != nil {
			return nil, fmt.Errorf(
				"invalid regex in [daemon].stuck_error_patterns[%d] %q: %w",
				i, src, err)
		}
		patterns = append(patterns, re)
	}
	return &memoryStuckTracker{
		wispThreshold: d.StuckWispThresholdDuration(),
		patterns:      patterns,
		peekLines:     d.StuckPeekLinesOrDefault(),
		warrantLabel:  d.StuckWarrantLabelOrDefault(),
	}, nil
}

// checkStuck implements the default "convergent evidence" composition:
// a session is stuck when its open wisp is stale AND at least one of the
// following holds:
//
//   - an error pattern matches recent pane output; or
//   - the session's last-activity time is older than the wisp
//     UpdatedAt by more than the wisp-staleness threshold (i.e., the
//     agent has produced I/O but no progress since the wisp opened).
//
// Fail-open semantics live in the caller: zero timestamps, empty pane
// output, or missing wisp-freshness data result in false (not stuck).
func (m *memoryStuckTracker) checkStuck(
	_ string,
	paneOutput string,
	wispUpdatedAt time.Time,
	lastActivity time.Time,
	now time.Time,
) (bool, string, string) {
	if wispUpdatedAt.IsZero() {
		return false, "", ""
	}
	wispAge := now.Sub(wispUpdatedAt)
	if wispAge <= m.wispThreshold {
		return false, "", ""
	}

	matched := m.matchPatterns(paneOutput)
	progressMismatch := false
	if !lastActivity.IsZero() && lastActivity.Before(wispUpdatedAt) {
		// Activity strictly older than wisp open — agent is producing no
		// I/O against the in-progress wisp.
		if now.Sub(lastActivity) > m.wispThreshold {
			progressMismatch = true
		}
	}

	if matched == "" && !progressMismatch {
		return false, "", ""
	}

	reason := buildStuckReason(wispAge, matched, progressMismatch)
	return true, reason, matched
}

// matchPatterns returns a deterministic, comma-joined string of regex
// sources that match paneOutput, or "" if none match. The list is sorted
// so repeated evaluations produce identical metadata.
func (m *memoryStuckTracker) matchPatterns(paneOutput string) string {
	if paneOutput == "" || len(m.patterns) == 0 {
		return ""
	}
	var hits []string
	for _, re := range m.patterns {
		if re.MatchString(paneOutput) {
			hits = append(hits, re.String())
		}
	}
	if len(hits) == 0 {
		return ""
	}
	sort.Strings(hits)
	return strings.Join(hits, ",")
}

// buildStuckReason formats a short human-readable reason string.
func buildStuckReason(wispAge time.Duration, matched string, progressMismatch bool) string {
	parts := []string{fmt.Sprintf("wisp stale %s", wispAge.Round(time.Second))}
	if matched != "" {
		parts = append(parts, fmt.Sprintf("pattern %q", matched))
	}
	if progressMismatch {
		parts = append(parts, "no progress since wisp opened")
	}
	return "stuck: " + strings.Join(parts, "; ")
}
