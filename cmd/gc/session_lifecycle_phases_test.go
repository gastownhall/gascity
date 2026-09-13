package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// TestStartPhaseTimingsFormatLog covers the formatLog helper that gc-67o
// uses to emit per-phase wall-clock segments in the session lifecycle log.
// Zero-valued phases must elide entirely so a healthy synchronous start
// without session_key still produces a single duration field.
func TestStartPhaseTimingsFormatLog(t *testing.T) {
	cases := []struct {
		name   string
		phases startPhaseTimings
		want   string
	}{
		{
			name:   "all zero elides",
			phases: startPhaseTimings{},
			want:   "",
		},
		{
			name: "start_call only",
			phases: startPhaseTimings{
				StartCall: 5 * time.Second,
			},
			want: " phases=[start_call=5s]",
		},
		{
			name: "start and post_start_observe",
			phases: startPhaseTimings{
				StartCall:        5 * time.Second,
				PostStartObserve: 2*time.Second + 100*time.Millisecond,
			},
			want: " phases=[start_call=5s post_start_observe=2.1s]",
		},
		{
			name: "all three phases",
			phases: startPhaseTimings{
				StartCall:        65 * time.Second,
				PostStartObserve: 2 * time.Second,
				CommitRefresh:    50 * time.Millisecond,
			},
			want: " phases=[start_call=1m5s post_start_observe=2s commit_refresh=50ms]",
		},
		{
			name: "state_sync_recovery emits when nonzero (gc-9ha)",
			phases: startPhaseTimings{
				StartCall:         82 * time.Second,
				StateSyncRecovery: 2 * time.Second,
				PostStartObserve:  2 * time.Second,
				CommitRefresh:     1 * time.Millisecond,
			},
			want: " phases=[start_call=1m22s state_sync_recovery=2s post_start_observe=2s commit_refresh=1ms]",
		},
		{
			name: "state_sync_recovery zero stays elided",
			phases: startPhaseTimings{
				StartCall:        4 * time.Second,
				PostStartObserve: 2 * time.Second,
			},
			want: " phases=[start_call=4s post_start_observe=2s]",
		},
		{
			name: "rounding to milliseconds",
			phases: startPhaseTimings{
				StartCall: 1234567 * time.Microsecond, // ~1.234567s
			},
			want: " phases=[start_call=1.235s]",
		},
		{
			name: "commit_refresh only (e.g. stale_async_start with no run)",
			phases: startPhaseTimings{
				CommitRefresh: 12 * time.Millisecond,
			},
			want: " phases=[commit_refresh=12ms]",
		},
		{
			// Regression: sub-0.5ms durations previously rendered as
			// "...=0s" because the >0 check ran on the unrounded value
			// but the printf used the rounded value. After the fix, the
			// rounded value drives the include decision and sub-ms is
			// elided entirely.
			name: "sub-millisecond phases elide (no =0s artifacts)",
			phases: startPhaseTimings{
				StartCall:        100 * time.Microsecond,
				PostStartObserve: 200 * time.Microsecond,
				CommitRefresh:    400 * time.Microsecond,
			},
			want: "",
		},
		{
			// Mix of sub-ms and ms+ durations: sub-ms phase elides, ms+ stays.
			name: "sub-ms phase elides while ms+ peer survives",
			phases: startPhaseTimings{
				StartCall:        300 * time.Microsecond, // <0.5ms → elide
				PostStartObserve: 5 * time.Second,
			},
			want: " phases=[post_start_observe=5s]",
		},
		{
			// Boundary: 500µs rounds to 1ms (Go's time.Duration.Round uses
			// half-away-from-zero), so it survives.
			name: "0.5ms boundary survives as 1ms",
			phases: startPhaseTimings{
				StartCall: 500 * time.Microsecond,
			},
			want: " phases=[start_call=1ms]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.phases.formatLog()
			if got != tc.want {
				t.Errorf("formatLog() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestLogLifecycleOutcomeWithPhases verifies that logLifecycleOutcome
// emits the phases segment when a single startPhaseTimings is supplied
// and omits it when none is. Existing call sites pass no variadic
// argument; this guards backward compatibility.
func TestLogLifecycleOutcomeWithPhases(t *testing.T) {
	started := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)
	finished := started.Add(7 * time.Second)

	t.Run("no phases (legacy callers)", func(t *testing.T) {
		var buf bytes.Buffer
		logLifecycleOutcome(&buf, "start", 0, "s-cos", "oversight-rig.cos", "success", started, finished, nil)
		got := buf.String()
		if !strings.Contains(got, "duration=7s") {
			t.Errorf("missing duration=7s in %q", got)
		}
		if strings.Contains(got, "phases=") {
			t.Errorf("legacy call should not emit phases segment: %q", got)
		}
	})

	t.Run("phases segment when supplied", func(t *testing.T) {
		var buf bytes.Buffer
		phases := startPhaseTimings{
			StartCall:        5 * time.Second,
			PostStartObserve: 2 * time.Second,
		}
		logLifecycleOutcome(&buf, "start", 0, "s-cos", "oversight-rig.cos", "success", started, finished, nil, phases)
		got := buf.String()
		if !strings.Contains(got, "phases=[start_call=5s post_start_observe=2s]") {
			t.Errorf("phases segment missing or malformed: %q", got)
		}
		if !strings.Contains(got, "duration=7s") {
			t.Errorf("missing duration=7s in %q", got)
		}
	})

	t.Run("zero phases tail elided", func(t *testing.T) {
		var buf bytes.Buffer
		// Pass an explicit zero startPhaseTimings — formatLog returns ""
		// so the log line should not contain a phases segment.
		logLifecycleOutcome(&buf, "start", 0, "s-cos", "oversight-rig.cos", "success", started, finished, nil, startPhaseTimings{})
		got := buf.String()
		if strings.Contains(got, "phases=") {
			t.Errorf("zero phases should not emit segment: %q", got)
		}
	})

	t.Run("error and phases coexist", func(t *testing.T) {
		var buf bytes.Buffer
		phases := startPhaseTimings{StartCall: 60 * time.Second}
		logLifecycleOutcome(&buf, "start", 0, "s-cos", "oversight-rig.cos", "deadline_exceeded", started, finished, errCtxDeadline{}, phases)
		got := buf.String()
		if !strings.Contains(got, "phases=[start_call=1m0s]") {
			t.Errorf("phases segment missing: %q", got)
		}
		if !strings.Contains(got, "err=ctx deadline") {
			t.Errorf("err segment missing: %q", got)
		}
	})
}

// errCtxDeadline is a tiny test-local error so the log-coexistence subtest
// doesn't pull in context.DeadlineExceeded indirectly.
type errCtxDeadline struct{}

func (errCtxDeadline) Error() string { return "ctx deadline" }
