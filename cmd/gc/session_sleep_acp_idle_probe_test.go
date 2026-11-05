package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionacp "github.com/gastownhall/gascity/internal/runtime/acp"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/testutil"
)

// acpSleepProvider builds the ACP provider the way production does, through
// the seam wrapper, so the sleep capability and idle interfaces under test are
// the real ones rather than a fake's.
func acpSleepProvider(t *testing.T) runtime.Provider {
	t.Helper()
	dir := filepath.Join(testutil.ShortTempDir(t, "gc-acp-sleep-"), "acp")
	return sessionacp.NewSeamBackedWithDir(dir, sessionacp.Config{})
}

// assertNoIdleProbeTarget runs the reconciler's probe selection over a single
// live session that is idle with no wake demand — the shape that gets probed
// when a session qualifies — and requires that nothing is selected.
func assertNoIdleProbeTarget(t *testing.T, info sessionpkg.Info, policy resolvedSessionSleepPolicy) {
	t.Helper()
	targets := []wakeTarget{{info: info, alive: true}}
	wakeEvals := map[string]wakeEvaluation{info.ID: {Policy: policy, ConfigSuppressed: true}}
	selected := selectIdleProbeTargets(targets, wakeEvals, newDrainTracker(), infoByIDForTargets(targets), time.Now())
	if len(selected) != 0 {
		t.Fatalf("selectIdleProbeTargets = %v, want no ACP probe target", selected)
	}
}

// TestACPSessionsNeverBecomeIdleProbeTargets pins the idle-sleep row of the
// behavior audit now that the ACP provider answers runtime.IdleWaitProvider.
// The new interface makes WaitForIdle return nil whenever no session/prompt is
// outstanding, which is exactly what the reconciler's idle-sleep probe asks —
// but it must not make an ACP session sleep eligible. Eligibility is decided
// by SleepCapability, which is timed-only for ACP, not by the interface
// assertion succeeding. Without this pin, arming idle sleep for ACP would be a
// silent side effect of a type assertion, applying a timeout tuned against the
// tmux predicate ("quiescent interactive pane") to a different claim ("no RPC
// in flight") that shares the same config knob.
func TestACPSessionsNeverBecomeIdleProbeTargets(t *testing.T) {
	sp := acpSleepProvider(t)
	if _, ok := sp.(runtime.IdleWaitProvider); !ok {
		t.Fatal("ACP provider no longer answers runtime.IdleWaitProvider; this test guards where that capability may not reach")
	}

	t.Run("interactive class is forced off by the timed-only capability", func(t *testing.T) {
		cfg := &config.City{
			SessionSleep: config.SessionSleepConfig{InteractiveResume: "60s"},
			Agents:       []config.Agent{{Name: "worker"}},
		}
		info := seedSessionInfo(makeBead("acp-interactive", map[string]string{
			"template":     "worker",
			"session_name": "gc-acp-interactive",
			"transport":    "acp",
		}))

		policy := resolveSessionSleepPolicyInfo(info, cfg, sp)
		if policy.Class != config.SessionSleepInteractiveResume {
			t.Fatalf("Class = %q, want %q", policy.Class, config.SessionSleepInteractiveResume)
		}
		if policy.Capability != runtime.SessionSleepCapabilityTimedOnly {
			t.Fatalf("Capability = %q, want %q", policy.Capability, runtime.SessionSleepCapabilityTimedOnly)
		}
		if policy.Effective != config.SessionSleepOff {
			t.Fatalf("Effective = %q, want %q", policy.Effective, config.SessionSleepOff)
		}
		if policy.AdjustmentReason != "interactive_capability_insufficient" {
			t.Fatalf("AdjustmentReason = %q, want interactive_capability_insufficient", policy.AdjustmentReason)
		}
		if policy.enabled() {
			t.Fatal("policy.enabled() = true for an ACP session; the timed-only capability must leave idle sleep off")
		}
		assertNoIdleProbeTarget(t, info, policy)
	})

	t.Run("non-interactive class is skipped by probe selection", func(t *testing.T) {
		cfg := &config.City{
			SessionSleep: config.SessionSleepConfig{NonInteractive: "60s"},
			Agents:       []config.Agent{{Name: "batch", Attach: boolPtr(false)}},
		}
		info := seedSessionInfo(makeBead("acp-noninteractive", map[string]string{
			"template":     "batch",
			"session_name": "gc-acp-noninteractive",
			"transport":    "acp",
		}))

		policy := resolveSessionSleepPolicyInfo(info, cfg, sp)
		if policy.Class != config.SessionSleepNonInteractive {
			t.Fatalf("Class = %q, want %q", policy.Class, config.SessionSleepNonInteractive)
		}
		// The non-interactive class keeps its configured value — it is the
		// class guard inside selectIdleProbeTargets, not an off policy, that
		// keeps these sessions away from the probe. Its drain decision
		// short-circuits ahead of the probe and is unchanged by this PR.
		if !policy.enabled() {
			t.Fatalf("Effective = %q, want the configured non-interactive value", policy.Effective)
		}
		assertNoIdleProbeTarget(t, info, policy)
	})

	t.Run("a successful probe does not open an ACP interactive drain", func(t *testing.T) {
		cfg := &config.City{
			SessionSleep: config.SessionSleepConfig{InteractiveResume: "60s"},
			Agents:       []config.Agent{{Name: "worker"}},
		}
		info := seedSessionInfo(makeBead("acp-probed", map[string]string{
			"template":     "worker",
			"session_name": "gc-acp-probed",
			"transport":    "acp",
		}))
		eval := wakeEvaluation{Policy: resolveSessionSleepPolicyInfo(info, cfg, sp), ConfigSuppressed: true}

		// Record the probe result the new WaitForIdle now produces for an idle
		// ACP session, so the capability gate is the only thing left deciding
		// the drain.
		dt := newDrainTracker()
		dt.finishIdleProbe(info.ID, dt.startIdleProbe(info.ID), true, time.Now().UTC())

		shouldBegin, err := shouldBeginIdleDrainInfo(info, eval, dt, sp)
		if err != nil {
			t.Fatalf("shouldBeginIdleDrainInfo: %v", err)
		}
		if shouldBegin {
			t.Fatal("shouldBeginIdleDrainInfo = true for an ACP session with a successful idle probe")
		}
	})
}
