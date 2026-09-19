package storehealth

import (
	"context"
	"testing"
	"time"
)

// recordingHooks captures every hook invocation so tests can assert the
// exact side-effect sequence the matrix produced.
type recordingHooks struct {
	aResult, bResult  ProbeResult
	writeResult       ProbeResult
	writeProbeSet     bool
	forensicsDir      string
	reapPIDs          int
	tripped           int
	degraded          []DegradeClass
	recovered         []DegradeClass
	probeFailed       []string
	reaped            []reapCall
	doctorAlerts      []string
	forensicsCaptured int
	reapCalled        int
	writeProbeCalled  int
}

type reapCall struct {
	dir         string
	pids        int
	rateLimited bool
}

func (h *recordingHooks) hooks() Hooks {
	hk := Hooks{
		ProbeRoutedFresh:   func(context.Context) ProbeResult { return h.aResult },
		ProbeBackendDirect: func(context.Context) ProbeResult { return h.bResult },
		CaptureForensics: func(context.Context) (string, error) {
			h.forensicsCaptured++
			return h.forensicsDir, nil
		},
		ReapProxy: func(context.Context) (int, error) {
			h.reapCalled++
			return h.reapPIDs, nil
		},
		TripBreaker:     func() { h.tripped++ },
		EmitDegraded:    func(c DegradeClass, _ string, _ int) { h.degraded = append(h.degraded, c) },
		EmitRecovered:   func(c DegradeClass) { h.recovered = append(h.recovered, c) },
		EmitProbeFailed: func(probe, _ string) { h.probeFailed = append(h.probeFailed, probe) },
		EmitProxyReaped: func(dir string, pids int, rl bool) {
			h.reaped = append(h.reaped, reapCall{dir: dir, pids: pids, rateLimited: rl})
		},
		EmitDoctorAlert: func(detail string) { h.doctorAlerts = append(h.doctorAlerts, detail) },
	}
	if h.writeProbeSet {
		hk.WriteProbe = func(context.Context) ProbeResult {
			h.writeProbeCalled++
			return h.writeResult
		}
	}
	return hk
}

func ok() ProbeResult   { return ProbeResult{Ok: true} }
func fail() ProbeResult { return ProbeResult{Ok: false, Reason: "boom"} }

// TestEvaluateCycleMatrix is the A/B decision-matrix table test.
func TestEvaluateCycleMatrix(t *testing.T) {
	tests := []struct {
		name          string
		a, b          ProbeResult
		wantTripped   int
		wantDegraded  []DegradeClass
		wantAlerts    int
		wantProbeFail int
	}{
		{"A-ok B-ok healthy", ok(), ok(), 0, nil, 0, 0},
		{"A-fail B-ok poison (single cycle, not yet confirmed)", fail(), ok(), 0, nil, 0, 1},
		{"A-ok B-fail backend fault", ok(), fail(), 1, []DegradeClass{ClassBackend}, 1, 1},
		{"A-fail B-fail both down", fail(), fail(), 1, []DegradeClass{ClassBackend}, 1, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := &recordingHooks{aResult: tc.a, bResult: tc.b}
			p := NewScopePatrol(Config{ConsecutiveFails: 3, ReapCooldown: 10 * time.Minute}, h.hooks(), fixedClock(0))
			p.EvaluateCycle(context.Background())
			if h.tripped != tc.wantTripped {
				t.Errorf("tripped = %d, want %d", h.tripped, tc.wantTripped)
			}
			if len(h.degraded) != len(tc.wantDegraded) {
				t.Errorf("degraded = %v, want %v", h.degraded, tc.wantDegraded)
			}
			for i := range tc.wantDegraded {
				if h.degraded[i] != tc.wantDegraded[i] {
					t.Errorf("degraded[%d] = %v, want %v", i, h.degraded[i], tc.wantDegraded[i])
				}
			}
			if len(h.doctorAlerts) != tc.wantAlerts {
				t.Errorf("doctorAlerts = %d, want %d", len(h.doctorAlerts), tc.wantAlerts)
			}
			if len(h.probeFailed) != tc.wantProbeFail {
				t.Errorf("probeFailed = %d, want %d", len(h.probeFailed), tc.wantProbeFail)
			}
		})
	}
}

// TestTransportPoisonConfirmAfterThreshold asserts a reap fires only after
// ConsecutiveFails consecutive A-fail∧B-ok cycles, forensics-first.
func TestTransportPoisonConfirmAfterThreshold(t *testing.T) {
	h := &recordingHooks{aResult: fail(), bResult: ok(), forensicsDir: "/q/scope-0", reapPIDs: 2}
	p := NewScopePatrol(Config{ConsecutiveFails: 3, ReapCooldown: 10 * time.Minute}, h.hooks(), fixedClock(0))

	// Cycles 1 and 2: counted, no reap yet.
	p.EvaluateCycle(context.Background())
	p.EvaluateCycle(context.Background())
	if h.reapCalled != 0 || h.forensicsCaptured != 0 {
		t.Fatalf("reap/forensics fired before threshold: reap=%d forensics=%d", h.reapCalled, h.forensicsCaptured)
	}
	if got := p.ConsecutiveTransportFails(); got != 2 {
		t.Fatalf("consecutive fails = %d, want 2", got)
	}

	// Cycle 3: confirmed → forensics FIRST, then reap, trip, degraded(transport), proxy.reaped.
	p.EvaluateCycle(context.Background())
	if h.forensicsCaptured != 1 {
		t.Fatalf("forensicsCaptured = %d, want 1", h.forensicsCaptured)
	}
	if h.reapCalled != 1 {
		t.Fatalf("reapCalled = %d, want 1", h.reapCalled)
	}
	if h.tripped != 1 {
		t.Fatalf("tripped = %d, want 1", h.tripped)
	}
	if len(h.degraded) != 1 || h.degraded[0] != ClassTransport {
		t.Fatalf("degraded = %v, want [transport]", h.degraded)
	}
	if len(h.reaped) != 1 || h.reaped[0].rateLimited || h.reaped[0].pids != 2 || h.reaped[0].dir != "/q/scope-0" {
		t.Fatalf("reaped = %+v, want one non-rate-limited reap of /q/scope-0 with 2 pids", h.reaped)
	}
	if p.Degraded() != ClassTransport {
		t.Fatalf("Degraded() = %q, want transport", p.Degraded())
	}
}

// TestReapRateLimitWithinCooldown asserts the reap is rate-limited across a
// sustained poison episode: the proxy is reaped once at first confirmation,
// not again inside the cooldown window, and once more after the window
// expires. Forensics, the breaker trip, and the degraded emission are
// once-per-episode (asserted in TestPoisonEpisodeEmitsOnceAcrossManyTicks);
// here we focus on the reap cadence.
func TestReapRateLimitWithinCooldown(t *testing.T) {
	clk := &mutClock{}
	h := &recordingHooks{aResult: fail(), bResult: ok(), forensicsDir: "/q/scope-1"}
	p := NewScopePatrol(Config{ConsecutiveFails: 1, ReapCooldown: 10 * time.Minute}, h.hooks(), clk.now)

	// First confirmed poison at t=0 → real reap, forensics captured once.
	p.EvaluateCycle(context.Background())
	if h.reapCalled != 1 || len(h.reaped) != 1 || h.reaped[0].rateLimited {
		t.Fatalf("first poison should reap non-rate-limited: reap=%d reaped=%+v", h.reapCalled, h.reaped)
	}
	if h.forensicsCaptured != 1 {
		t.Fatalf("forensicsCaptured = %d after first confirmation, want 1", h.forensicsCaptured)
	}

	// Second poison 5m later (inside 10m cooldown), episode already confirmed
	// → no reap, no extra forensics, no extra reap event.
	clk.advance(5 * time.Minute)
	p.EvaluateCycle(context.Background())
	if h.reapCalled != 1 {
		t.Fatalf("reap called again inside cooldown: reapCalled = %d, want 1", h.reapCalled)
	}
	if h.forensicsCaptured != 1 {
		t.Fatalf("forensics re-captured inside cooldown for a confirmed episode: forensicsCaptured = %d, want 1", h.forensicsCaptured)
	}
	if len(h.reaped) != 1 {
		t.Fatalf("extra reap event emitted inside cooldown: reaped = %+v, want 1", h.reaped)
	}

	// A third poison after the cooldown expires → real reap again (the proxy
	// is still poisoned and the window has elapsed).
	clk.advance(6 * time.Minute) // now t=11m, > 10m after the t=0 reap
	p.EvaluateCycle(context.Background())
	if h.reapCalled != 2 {
		t.Fatalf("reap should fire after cooldown expiry: reapCalled = %d, want 2", h.reapCalled)
	}
}

// TestPoisonEpisodeEmitsOnceAcrossManyTicks asserts that a sustained poison
// across many patrol ticks captures forensics, trips the breaker, and emits
// degraded exactly ONCE per episode — only the per-tick probe-failed
// breadcrumb repeats — and that a genuine recovery followed by a re-poison
// re-captures.
func TestPoisonEpisodeEmitsOnceAcrossManyTicks(t *testing.T) {
	clk := &mutClock{}
	h := &recordingHooks{aResult: fail(), bResult: ok(), forensicsDir: "/q/ep"}
	// Large cooldown so the reap never re-fires; we are asserting the
	// once-per-episode forensics/trip/degraded, isolated from the reap cadence.
	p := NewScopePatrol(Config{ConsecutiveFails: 3, ReapCooldown: time.Hour}, h.hooks(), clk.now)

	// 20 poison ticks at a 30s interval (a multi-minute outage).
	for i := 0; i < 20; i++ {
		p.EvaluateCycle(context.Background())
		clk.advance(30 * time.Second)
	}
	if h.forensicsCaptured != 1 {
		t.Fatalf("forensicsCaptured = %d across a sustained episode, want 1", h.forensicsCaptured)
	}
	if h.tripped != 1 {
		t.Fatalf("tripped = %d across a sustained episode, want 1", h.tripped)
	}
	if len(h.degraded) != 1 || h.degraded[0] != ClassTransport {
		t.Fatalf("degraded = %v across a sustained episode, want exactly [transport]", h.degraded)
	}
	// The per-tick breadcrumb survives: one routed probe-failed per cycle.
	if len(h.probeFailed) != 20 {
		t.Fatalf("probeFailed = %d, want 20 (one per tick — the per-cycle signal must survive)", len(h.probeFailed))
	}

	// Recovery clears the episode.
	h.aResult = ok()
	p.EvaluateCycle(context.Background())
	clk.advance(30 * time.Second)
	if p.Degraded() != "" || len(h.recovered) != 1 {
		t.Fatalf("recovery not registered: degraded=%q recovered=%v", p.Degraded(), h.recovered)
	}

	// Re-poison after recovery → a NEW episode re-captures and re-emits.
	h.aResult = fail()
	for i := 0; i < 3; i++ {
		p.EvaluateCycle(context.Background())
		clk.advance(30 * time.Second)
	}
	if h.forensicsCaptured != 2 {
		t.Fatalf("forensicsCaptured = %d after recovery→re-poison, want 2 (new episode re-captures)", h.forensicsCaptured)
	}
	if h.tripped != 2 {
		t.Fatalf("tripped = %d after recovery→re-poison, want 2", h.tripped)
	}
	if len(h.degraded) != 2 {
		t.Fatalf("degraded = %v after recovery→re-poison, want 2 emissions", h.degraded)
	}
}

// TestHealthyAfterDegradedEmitsRecovered asserts recovery emission and
// counter reset when a degraded scope's probes pass again.
func TestHealthyAfterDegradedEmitsRecovered(t *testing.T) {
	h := &recordingHooks{aResult: fail(), bResult: ok()}
	p := NewScopePatrol(Config{ConsecutiveFails: 1, ReapCooldown: time.Minute}, h.hooks(), fixedClock(0))
	p.EvaluateCycle(context.Background()) // degrade transport
	if p.Degraded() != ClassTransport {
		t.Fatalf("expected transport degradation, got %q", p.Degraded())
	}

	h.aResult = ok()
	p.EvaluateCycle(context.Background()) // healthy
	if p.Degraded() != "" {
		t.Fatalf("Degraded() = %q after recovery, want empty", p.Degraded())
	}
	if len(h.recovered) != 1 || h.recovered[0] != ClassTransport {
		t.Fatalf("recovered = %v, want [transport]", h.recovered)
	}
	if p.ConsecutiveTransportFails() != 0 {
		t.Fatalf("consecutive fails not reset: %d", p.ConsecutiveTransportFails())
	}
}

// TestWriteProbeRejectionDegrades asserts the write-path conformance probe
// degrades with class write-rejection (the a74fefde8 runtime class), and
// that a later passing probe clears it.
func TestWriteProbeRejectionDegrades(t *testing.T) {
	clk := &mutClock{}
	h := &recordingHooks{aResult: ok(), bResult: ok(), writeProbeSet: true, writeResult: fail()}
	p := NewScopePatrol(Config{ConsecutiveFails: 3, WriteProbeEvery: 10 * time.Minute}, h.hooks(), clk.now)

	p.EvaluateCycle(context.Background()) // first cycle: write probe due, rejects
	if h.writeProbeCalled != 1 {
		t.Fatalf("writeProbeCalled = %d, want 1", h.writeProbeCalled)
	}
	if p.Degraded() != ClassWriteRejection {
		t.Fatalf("Degraded() = %q, want write-rejection", p.Degraded())
	}
	if len(h.degraded) != 1 || h.degraded[0] != ClassWriteRejection {
		t.Fatalf("degraded = %v, want [write-rejection]", h.degraded)
	}

	// Not yet due again (within 10m) → no extra probe.
	clk.advance(5 * time.Minute)
	p.EvaluateCycle(context.Background())
	if h.writeProbeCalled != 1 {
		t.Fatalf("write probe ran before its interval: writeProbeCalled = %d, want 1", h.writeProbeCalled)
	}

	// Due again, now passes → clears write-rejection, emits recovered.
	clk.advance(6 * time.Minute)
	h.writeResult = ok()
	p.EvaluateCycle(context.Background())
	if h.writeProbeCalled != 2 {
		t.Fatalf("writeProbeCalled = %d, want 2", h.writeProbeCalled)
	}
	if p.Degraded() != "" {
		t.Fatalf("Degraded() = %q after write recovery, want empty", p.Degraded())
	}
	if len(h.recovered) != 1 || h.recovered[0] != ClassWriteRejection {
		t.Fatalf("recovered = %v, want [write-rejection]", h.recovered)
	}
}

// --- deterministic clocks ---

func fixedClock(t time.Duration) func() time.Time {
	base := time.Unix(0, 0).Add(t)
	return func() time.Time { return base }
}

type mutClock struct {
	d time.Duration
}

func (c *mutClock) now() time.Time          { return time.Unix(0, 0).Add(c.d) }
func (c *mutClock) advance(d time.Duration) { c.d += d }
