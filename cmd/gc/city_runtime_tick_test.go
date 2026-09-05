package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

// decodeTickPayload decodes one controller.tick_completed event's payload.
func decodeTickPayload(t *testing.T, e events.Event) events.ControllerTickCompletedPayload {
	t.Helper()
	if e.Type != events.ControllerTickCompleted {
		t.Fatalf("event type = %q, want %q", e.Type, events.ControllerTickCompleted)
	}
	decoded, _, err := events.DecodePayload(e.Type, e.Payload)
	if err != nil {
		t.Fatalf("decode tick payload: %v", err)
	}
	p, ok := decoded.(events.ControllerTickCompletedPayload)
	if !ok {
		t.Fatalf("decoded payload type %T, want ControllerTickCompletedPayload", decoded)
	}
	return p
}

// TestRecordTickHeartbeatEmitsEveryTick asserts the heartbeat stream is
// unsampled: one event per completed tick, fast or slow (vp-qvqk defect 2
// — the old breach-or-every-10th gate made the stream a biased sample, so
// period arithmetic over it was valid only while every tick breached).
func TestRecordTickHeartbeatEmitsEveryTick(t *testing.T) {
	ep := events.NewFake()
	cfg := &config.City{}
	cfg.Daemon.PatrolInterval = "10s"
	cr := &CityRuntime{cityName: "testcity", cfg: cfg, rec: ep}

	// 25 fast ticks: under the old sampling only ticks 10 and 20 would
	// emit; every one of these is far below any breach threshold.
	for i := 0; i < 25; i++ {
		cr.recordTickHeartbeat("patrol", 10*time.Millisecond)
	}

	if got := len(ep.Events); got != 25 {
		t.Fatalf("events emitted = %d, want 25 (one per tick, no sampling)", got)
	}
	for i, e := range ep.Events {
		p := decodeTickPayload(t, e)
		if p.ThresholdBreach {
			t.Fatalf("event %d: ThresholdBreach = true for a 10ms tick at 10s patrol", i)
		}
		if p.Phase != "patrol" {
			t.Errorf("event %d: phase = %q, want patrol", i, p.Phase)
		}
	}
}

// TestRecordTickHeartbeatBreachRelativeToPatrolInterval asserts the
// slow-tick flag is calibrated to the configured cadence, not an absolute
// constant (vp-qvqk defect 1 — a fixed 5s threshold in a 30-55s tick
// regime was ON for 100% of ticks and carried zero bits).
func TestRecordTickHeartbeatBreachRelativeToPatrolInterval(t *testing.T) {
	tests := []struct {
		name       string
		interval   string
		dur        time.Duration
		wantBreach bool
	}{
		// 10s patrol → 20s threshold.
		{"under threshold", "10s", 19 * time.Second, false},
		{"at threshold", "10s", 20 * time.Second, true},
		{"past threshold", "10s", 443 * time.Second, true},
		// The vc-wz5 regime: 30-55s ticks at a 30s patrol must NOT breach —
		// under the old absolute 5s constant every one of them did.
		{"steady 55s tick at 30s patrol", "30s", 55 * time.Second, false},
		{"excursion at 30s patrol", "30s", 61 * time.Second, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ep := events.NewFake()
			cfg := &config.City{}
			cfg.Daemon.PatrolInterval = tc.interval
			cr := &CityRuntime{cityName: "testcity", cfg: cfg, rec: ep}

			cr.recordTickHeartbeat("patrol", tc.dur)

			if len(ep.Events) != 1 {
				t.Fatalf("events emitted = %d, want 1", len(ep.Events))
			}
			p := decodeTickPayload(t, ep.Events[0])
			if p.ThresholdBreach != tc.wantBreach {
				t.Fatalf("ThresholdBreach = %v, want %v (dur=%s interval=%s)", p.ThresholdBreach, tc.wantBreach, tc.dur, tc.interval)
			}
			if p.DurationMs != tc.dur.Milliseconds() {
				t.Errorf("DurationMs = %d, want %d", p.DurationMs, tc.dur.Milliseconds())
			}
		})
	}
}

// TestSlowTickThresholdFallback asserts the threshold degrades to the
// legacy absolute rather than to zero (which would make breach
// always-true, the exact defect this replaces).
func TestSlowTickThresholdFallback(t *testing.T) {
	nilCfg := &CityRuntime{}
	if got := nilCfg.slowTickThreshold(); got != tickSlowFallbackThreshold {
		t.Fatalf("nil cfg threshold = %s, want %s", got, tickSlowFallbackThreshold)
	}

	cfg := &config.City{}
	cfg.Daemon.PatrolInterval = "10s"
	cr := &CityRuntime{cfg: cfg}
	want := time.Duration(tickSlowIntervalMultiple) * 10 * time.Second
	if got := cr.slowTickThreshold(); got != want {
		t.Fatalf("threshold = %s, want %s (%d× patrol)", got, want, tickSlowIntervalMultiple)
	}
}
