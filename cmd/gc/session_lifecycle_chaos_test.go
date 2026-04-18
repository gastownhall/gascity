package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestSessionLifecycleChaos keeps the default package run bounded and
// replayable. Use GC_SESSION_CHAOS_SEED, GC_SESSION_CHAOS_ITERS, and
// GC_SESSION_CHAOS_STEPS to replay or extend a run; set
// GC_SESSION_CHAOS_NIGHTLY=1 for a longer bounded preset.
func TestSessionLifecycleChaos(t *testing.T) {
	runSessionLifecycleChaos(t, sessionChaosOptions{})
}

func TestSessionLifecycleChaosPendingInteractionDoesNotOverrideOrphanDrain(t *testing.T) {
	h := newSessionChaosHarness(t, 20260415)
	h.createSessionIntent()
	h.assertCreatingIntent()
	h.reconcileTick()
	h.assertStarted()

	h.setDesired(false)
	h.env.sp.SetPendingInteraction(h.sessionName, &runtime.PendingInteraction{
		RequestID: "chaos-pending",
		Kind:      "question",
		Prompt:    "chaos test",
	})
	h.record("pending interaction with demand removed")
	h.reconcileTick()

	if ds := h.env.dt.get(h.sessionID); ds == nil || ds.reason != "orphaned" {
		h.failf("pending interaction bypassed orphan drain: %+v", ds)
	}
	if !h.env.sp.IsRunning(h.sessionName) {
		h.failf("orphan drain stopped runtime %q before acknowledgement", h.sessionName)
	}
}

func TestSessionLifecycleChaosPendingInteractionDoesNotOverrideSuspendedDrain(t *testing.T) {
	h := newSessionChaosHarness(t, 20260427)
	h.createSessionIntent()
	h.assertCreatingIntent()
	h.reconcileTick()
	h.assertStarted()

	h.env.cfg.NamedSessions = []config.NamedSession{{
		Name:     h.sessionName,
		Template: h.template,
	}}
	h.setDesired(false)
	h.env.sp.SetPendingInteraction(h.sessionName, &runtime.PendingInteraction{
		RequestID: "chaos-pending-suspended",
		Kind:      "question",
		Prompt:    "chaos test",
	})
	h.record("pending interaction with suspended session")
	h.reconcileTick()

	if ds := h.env.dt.get(h.sessionID); ds == nil || ds.reason != "suspended" {
		h.failf("pending interaction bypassed suspended drain: %+v", ds)
	}
	if !h.env.sp.IsRunning(h.sessionName) {
		h.failf("suspended drain stopped runtime %q before acknowledgement", h.sessionName)
	}
}

func TestSessionLifecycleChaosPendingInteractionCancelsExistingCancelableDrain(t *testing.T) {
	h := newSessionChaosHarness(t, 20260416)
	h.createSessionIntent()
	h.assertCreatingIntent()
	h.reconcileTick()
	h.assertStarted()

	beginSessionDrain(h.mustBead(), h.env.sp, h.env.dt, "idle", h.env.clk, defaultDrainTimeout)
	if ds := h.env.dt.get(h.sessionID); ds == nil || ds.reason != "idle" {
		h.failf("expected idle drain before pending interaction, got %+v", ds)
	}

	h.env.sp.SetPendingInteraction(h.sessionName, &runtime.PendingInteraction{
		RequestID: "chaos-pending-after-drain",
		Kind:      "question",
		Prompt:    "chaos test",
	})
	h.record("pending interaction after drain started")
	h.reconcileTick()

	if ds := h.env.dt.get(h.sessionID); ds != nil {
		h.failf("pending interaction left existing drain active: %+v", *ds)
	}
	if ack, _ := h.env.sp.GetMeta(h.sessionName, "GC_DRAIN_ACK"); ack != "" {
		h.failf("pending interaction left GC_DRAIN_ACK=%q", ack)
	}
	if drain, _ := h.env.sp.GetMeta(h.sessionName, "GC_DRAIN"); drain != "" {
		h.failf("pending interaction left GC_DRAIN=%q", drain)
	}
}

func TestSessionLifecycleChaosPendingInteractionPreservesExplicitDrainRequest(t *testing.T) {
	h := newSessionChaosHarness(t, 20260437)
	h.createSessionIntent()
	h.assertCreatingIntent()
	h.reconcileTick()
	h.assertStarted()

	beginSessionDrain(h.mustBead(), h.env.sp, h.env.dt, "idle", h.env.clk, defaultDrainTimeout)
	if err := h.env.sp.SetMeta(h.sessionName, "GC_DRAIN", "manual"); err != nil {
		h.failf("set explicit GC_DRAIN: %v", err)
	}
	h.env.sp.SetPendingInteraction(h.sessionName, &runtime.PendingInteraction{
		RequestID: "chaos-pending-explicit-drain",
		Kind:      "question",
		Prompt:    "chaos test",
	})
	h.record("pending interaction with explicit drain request")
	h.reconcileTick()

	if ds := h.env.dt.get(h.sessionID); ds != nil {
		h.failf("pending interaction left cancelable drain active: %+v", *ds)
	}
	if drain, _ := h.env.sp.GetMeta(h.sessionName, "GC_DRAIN"); drain != "manual" {
		h.failf("pending interaction cleared explicit GC_DRAIN=%q", drain)
	}
}

func TestSessionLifecycleChaosPendingInteractionClearsReconcilerDrainAckBeforeStop(t *testing.T) {
	h := newSessionChaosHarness(t, 20260426)
	h.createSessionIntent()
	h.assertCreatingIntent()
	h.reconcileTick()
	h.assertStarted()

	beginSessionDrain(h.mustBead(), h.env.sp, h.env.dt, "idle", h.env.clk, defaultDrainTimeout)
	ds := h.env.dt.get(h.sessionID)
	if ds == nil {
		h.failf("expected idle drain before pending interaction")
	}
	ds.ackSet = true
	if err := setReconcilerDrainAckMetadata(h.env.sp, h.sessionName, ds); err != nil {
		h.failf("set reconciler drain ack: %v", err)
	}

	h.env.sp.SetPendingInteraction(h.sessionName, &runtime.PendingInteraction{
		RequestID: "chaos-pending-after-reconciler-ack",
		Kind:      "question",
		Prompt:    "chaos test",
	})
	h.record("pending interaction after reconciler drain ack")
	h.reconcileTickWithDrainOps()

	if !h.env.sp.IsRunning(h.sessionName) {
		h.failf("pending interaction did not preempt reconciler drain ack for %q", h.sessionName)
	}
	if ds := h.env.dt.get(h.sessionID); ds != nil {
		h.failf("pending interaction left reconciler-acked drain active: %+v", *ds)
	}
	if ack, _ := h.env.sp.GetMeta(h.sessionName, "GC_DRAIN_ACK"); ack != "" {
		h.failf("pending interaction left GC_DRAIN_ACK=%q", ack)
	}
	if drain, _ := h.env.sp.GetMeta(h.sessionName, "GC_DRAIN"); drain != "" {
		h.failf("pending interaction left GC_DRAIN=%q", drain)
	}
}

func TestSessionLifecycleChaosPendingInteractionClearsRecoveredReconcilerDrainAckBeforeStop(t *testing.T) {
	h := newSessionChaosHarness(t, 20260428)
	h.createSessionIntent()
	h.assertCreatingIntent()
	h.reconcileTick()
	h.assertStarted()

	beginSessionDrain(h.mustBead(), h.env.sp, h.env.dt, "idle", h.env.clk, defaultDrainTimeout)
	ds := h.env.dt.get(h.sessionID)
	if ds == nil {
		h.failf("expected idle drain before pending interaction")
	}
	if err := setReconcilerDrainAckMetadata(h.env.sp, h.sessionName, ds); err != nil {
		h.failf("set reconciler drain ack: %v", err)
	}
	h.env.dt = newDrainTracker()

	h.env.sp.SetPendingInteraction(h.sessionName, &runtime.PendingInteraction{
		RequestID: "chaos-pending-after-recovered-reconciler-ack",
		Kind:      "question",
		Prompt:    "chaos test",
	})
	h.record("pending interaction after recovered reconciler drain ack")
	h.reconcileTickWithDrainOps()

	if !h.env.sp.IsRunning(h.sessionName) {
		h.failf("pending interaction did not preempt recovered reconciler drain ack for %q", h.sessionName)
	}
	if ack, _ := h.env.sp.GetMeta(h.sessionName, "GC_DRAIN_ACK"); ack != "" {
		h.failf("pending interaction left recovered GC_DRAIN_ACK=%q", ack)
	}
	if source, _ := h.env.sp.GetMeta(h.sessionName, reconcilerDrainAckSourceKey); source != "" {
		h.failf("pending interaction left recovered %s=%q", reconcilerDrainAckSourceKey, source)
	}
}

func TestSessionLifecycleChaosClearsStaleRecoveredReconcilerDrainAck(t *testing.T) {
	h := newSessionChaosHarness(t, 20260436)
	h.createSessionIntent()
	h.assertCreatingIntent()
	h.reconcileTick()
	h.assertStarted()

	beginSessionDrain(h.mustBead(), h.env.sp, h.env.dt, "idle", h.env.clk, defaultDrainTimeout)
	ds := h.env.dt.get(h.sessionID)
	if ds == nil {
		h.failf("expected idle drain before stale ack setup")
	}
	if err := setReconcilerDrainAckMetadata(h.env.sp, h.sessionName, ds); err != nil {
		h.failf("set reconciler drain ack: %v", err)
	}
	if err := h.env.store.SetMetadata(h.sessionID, "generation", strconv.Itoa(ds.generation+1)); err != nil {
		h.failf("bump generation: %v", err)
	}
	h.env.dt = newDrainTracker()

	h.record("stale recovered reconciler drain ack before phase1")
	h.reconcileTickWithDrainOps()

	if !h.env.sp.IsRunning(h.sessionName) {
		h.failf("stale recovered reconciler drain ack stopped fresh runtime %q", h.sessionName)
	}
	if ack, _ := h.env.sp.GetMeta(h.sessionName, "GC_DRAIN_ACK"); ack != "" {
		h.failf("stale recovered reconciler drain ack left GC_DRAIN_ACK=%q", ack)
	}
	if source, _ := h.env.sp.GetMeta(h.sessionName, reconcilerDrainAckSourceKey); source != "" {
		h.failf("stale recovered reconciler drain ack left %s=%q", reconcilerDrainAckSourceKey, source)
	}
}

func TestSessionLifecycleChaosStartClearsLegacyDrainAck(t *testing.T) {
	h := newSessionChaosHarness(t, 20260438)
	h.createSessionIntent()
	h.assertCreatingIntent()
	if err := h.env.sp.SetMeta(h.sessionName, "GC_DRAIN_ACK", "1"); err != nil {
		h.failf("set legacy GC_DRAIN_ACK: %v", err)
	}
	if err := h.env.sp.SetMeta(h.sessionName, "GC_DRAIN", "manual"); err != nil {
		h.failf("set explicit GC_DRAIN: %v", err)
	}

	h.record("legacy drain ack before successful start")
	h.reconcileTick()
	h.assertStarted()

	if ack, _ := h.env.sp.GetMeta(h.sessionName, "GC_DRAIN_ACK"); ack != "" {
		h.failf("successful start left legacy GC_DRAIN_ACK=%q", ack)
	}
	if drain, _ := h.env.sp.GetMeta(h.sessionName, "GC_DRAIN"); drain != "manual" {
		h.failf("successful start cleared explicit GC_DRAIN=%q", drain)
	}
	h.reconcileTickWithDrainOps()
	if !h.env.sp.IsRunning(h.sessionName) {
		h.failf("legacy GC_DRAIN_ACK stopped fresh runtime %q after start", h.sessionName)
	}
}

func TestSessionLifecycleChaosPendingInteractionDoesNotCancelAgentDrainAck(t *testing.T) {
	h := newSessionChaosHarness(t, 20260429)
	h.createSessionIntent()
	h.assertCreatingIntent()
	h.reconcileTick()
	h.assertStarted()

	if err := h.env.sp.SetMeta(h.sessionName, "GC_DRAIN_ACK", "1"); err != nil {
		h.failf("set agent GC_DRAIN_ACK: %v", err)
	}
	h.env.sp.SetPendingInteraction(h.sessionName, &runtime.PendingInteraction{
		RequestID: "chaos-pending-after-agent-ack",
		Kind:      "question",
		Prompt:    "chaos test",
	})
	h.record("pending interaction after agent drain ack")
	h.reconcileTickWithDrainOps()

	if h.env.sp.IsRunning(h.sessionName) {
		h.failf("pending interaction canceled agent-authored drain ack for %q", h.sessionName)
	}
}

func TestSessionLifecycleChaosAgentDrainAckClearsRecoveredReconcilerProvenance(t *testing.T) {
	h := newSessionChaosHarness(t, 20260430)
	h.createSessionIntent()
	h.assertCreatingIntent()
	h.reconcileTick()
	h.assertStarted()

	beginSessionDrain(h.mustBead(), h.env.sp, h.env.dt, "idle", h.env.clk, defaultDrainTimeout)
	ds := h.env.dt.get(h.sessionID)
	if ds == nil {
		h.failf("expected idle drain before agent drain ack")
	}
	if err := setReconcilerDrainAckMetadata(h.env.sp, h.sessionName, ds); err != nil {
		h.failf("set reconciler drain ack: %v", err)
	}
	if err := newDrainOps(h.env.sp).setDrainAck(h.sessionName); err != nil {
		h.failf("agent drain ack: %v", err)
	}
	if source, _ := h.env.sp.GetMeta(h.sessionName, reconcilerDrainAckSourceKey); source != drainAckSourceAgentValue {
		h.failf("agent drain ack source = %q, want %q", source, drainAckSourceAgentValue)
	}
	h.env.dt = newDrainTracker()

	h.env.sp.SetPendingInteraction(h.sessionName, &runtime.PendingInteraction{
		RequestID: "chaos-pending-after-agent-overwrite",
		Kind:      "question",
		Prompt:    "chaos test",
	})
	h.record("pending interaction after agent drain ack overwrote provenance")
	h.reconcileTickWithDrainOps()

	if h.env.sp.IsRunning(h.sessionName) {
		h.failf("pending interaction canceled agent-authored drain ack after provenance overwrite for %q", h.sessionName)
	}
}

func TestSessionLifecycleChaosAgentDrainAckClearsLiveControllerDrain(t *testing.T) {
	h := newSessionChaosHarness(t, 20260432)
	h.createSessionIntent()
	h.assertCreatingIntent()
	h.reconcileTick()
	h.assertStarted()

	beginSessionDrain(h.mustBead(), h.env.sp, h.env.dt, "idle", h.env.clk, defaultDrainTimeout)
	ds := h.env.dt.get(h.sessionID)
	if ds == nil {
		h.failf("expected idle drain before agent drain ack")
	}
	ds.ackSet = true
	if err := setReconcilerDrainAckMetadata(h.env.sp, h.sessionName, ds); err != nil {
		h.failf("set reconciler drain ack: %v", err)
	}
	if err := newDrainOps(h.env.sp).setDrainAck(h.sessionName); err != nil {
		h.failf("agent drain ack: %v", err)
	}

	h.record("agent drain ack while controller drain remains in memory")
	h.reconcileTickWithDrainOps()

	if h.env.sp.IsRunning(h.sessionName) {
		h.failf("agent-authored drain ack left runtime %q running", h.sessionName)
	}
	if ds := h.env.dt.get(h.sessionID); ds != nil {
		h.failf("agent-authored drain ack left controller drain active: %+v", *ds)
	}
	b := h.mustBead()
	if got := b.Metadata["state"]; got != string(sessionpkg.StateDrained) {
		h.failf("agent-authored drain ack state = %q, want drained", got)
	}
}

func TestSessionLifecycleChaosAgentDrainAckStopFailurePreservesRetry(t *testing.T) {
	h := newSessionChaosHarness(t, 20260433)
	h.createSessionIntent()
	h.assertCreatingIntent()
	h.reconcileTick()
	h.assertStarted()

	beginSessionDrain(h.mustBead(), h.env.sp, h.env.dt, "idle", h.env.clk, defaultDrainTimeout)
	ds := h.env.dt.get(h.sessionID)
	if ds == nil {
		h.failf("expected idle drain before agent drain ack")
	}
	ds.ackSet = true
	if err := setReconcilerDrainAckMetadata(h.env.sp, h.sessionName, ds); err != nil {
		h.failf("set reconciler drain ack: %v", err)
	}
	if err := newDrainOps(h.env.sp).setDrainAck(h.sessionName); err != nil {
		h.failf("agent drain ack: %v", err)
	}
	h.env.sp.StopErrors[h.sessionName] = errors.New("chaos stop failure")

	h.record("agent drain ack with transient stop failure")
	h.reconcileTickWithDrainOps()

	if !h.env.sp.IsRunning(h.sessionName) {
		h.failf("stop failure removed runtime %q", h.sessionName)
	}
	if ack, _ := h.env.sp.GetMeta(h.sessionName, "GC_DRAIN_ACK"); ack != "1" {
		h.failf("stop failure cleared agent GC_DRAIN_ACK=%q", ack)
	}
	if ds := h.env.dt.get(h.sessionID); ds != nil {
		h.failf("stop failure left cancelable controller drain active after agent ack: %+v", *ds)
	}

	delete(h.env.sp.StopErrors, h.sessionName)
	h.record("agent drain ack retry after stop recovers")
	h.reconcileTickWithDrainOps()

	if h.env.sp.IsRunning(h.sessionName) {
		h.failf("retried agent drain ack left runtime %q running", h.sessionName)
	}
	if ds := h.env.dt.get(h.sessionID); ds != nil {
		h.failf("retried agent drain ack left controller drain active: %+v", *ds)
	}
}

func TestSessionLifecycleChaosPendingInteractionPreservesNonCancelableDrains(t *testing.T) {
	cases := []struct {
		name  string
		seed  int64
		setup func(*sessionChaosHarness)
	}{
		{
			name: "orphaned",
			seed: 20260424,
			setup: func(h *sessionChaosHarness) {
				h.setDesired(false)
			},
		},
		{
			name: "suspended",
			seed: 20260425,
			setup: func(h *sessionChaosHarness) {
				h.env.cfg.NamedSessions = []config.NamedSession{{
					Name:     h.sessionName,
					Template: h.template,
				}}
				h.setDesired(false)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newSessionChaosHarness(t, tc.seed)
			h.createSessionIntent()
			h.assertCreatingIntent()
			h.reconcileTick()
			h.assertStarted()

			tc.setup(h)
			h.record("start non-cancelable drain=%s", tc.name)
			h.reconcileTick()
			if ds := h.env.dt.get(h.sessionID); ds == nil || ds.reason != tc.name {
				h.failf("expected %s drain before pending interaction, got %+v", tc.name, ds)
			}

			h.env.sp.SetPendingInteraction(h.sessionName, &runtime.PendingInteraction{
				RequestID: "chaos-pending-non-cancelable-" + tc.name,
				Kind:      "question",
				Prompt:    "chaos test",
			})
			h.record("pending interaction after non-cancelable drain=%s", tc.name)
			h.reconcileTick()

			if ds := h.env.dt.get(h.sessionID); ds == nil || ds.reason != tc.name {
				h.failf("pending interaction canceled %s drain: %+v", tc.name, ds)
			}
		})
	}
}

func TestSessionLifecycleChaosPendingInteractionCancelsExistingConfigDriftDrain(t *testing.T) {
	h := newSessionChaosHarness(t, 20260439)
	h.createSessionIntent()
	h.assertCreatingIntent()
	h.reconcileTick()
	h.assertStarted()

	beginSessionDrain(h.mustBead(), h.env.sp, h.env.dt, "config-drift", h.env.clk, defaultDrainTimeout)
	if ds := h.env.dt.get(h.sessionID); ds == nil || ds.reason != "config-drift" {
		h.failf("expected config-drift drain before pending interaction, got %+v", ds)
	}

	h.env.sp.SetPendingInteraction(h.sessionName, &runtime.PendingInteraction{
		RequestID: "chaos-pending-config-drift-existing",
		Kind:      "question",
		Prompt:    "chaos test",
	})
	h.record("pending interaction after config-drift drain")
	h.reconcileTick()

	if ds := h.env.dt.get(h.sessionID); ds != nil {
		h.failf("pending interaction left config-drift drain active: %+v", *ds)
	}
	if !h.env.sp.IsRunning(h.sessionName) {
		h.failf("pending interaction stopped config-drift runtime %q", h.sessionName)
	}
}

func TestSessionLifecycleChaosPendingInteractionDefersConfigDrift(t *testing.T) {
	h := newSessionChaosHarness(t, 20260417)
	h.createSessionIntent()
	h.assertCreatingIntent()
	h.reconcileTick()
	h.assertStarted()

	h.env.sp.SetPendingInteraction(h.sessionName, &runtime.PendingInteraction{
		RequestID: "chaos-pending-config-drift",
		Kind:      "question",
		Prompt:    "chaos test",
	})
	h.command = "chaos-cmd-v2"
	h.setDesired(true)
	h.record("pending interaction before config drift")
	h.reconcileTick()

	if ds := h.env.dt.get(h.sessionID); ds != nil {
		h.failf("pending interaction did not defer config-drift drain: %+v", *ds)
	}
	if !h.env.sp.IsRunning(h.sessionName) {
		h.failf("pending interaction did not keep drifted runtime %q running", h.sessionName)
	}
}

func TestSessionLifecycleChaosPendingInteractionDefersIdleTimeout(t *testing.T) {
	h := newSessionChaosHarness(t, 20260418)
	h.createSessionIntent()
	h.assertCreatingIntent()
	h.reconcileTick()
	h.assertStarted()

	h.env.sp.SetPendingInteraction(h.sessionName, &runtime.PendingInteraction{
		RequestID: "chaos-pending-idle-timeout",
		Kind:      "question",
		Prompt:    "chaos test",
	})
	it := newFakeIdleTracker()
	it.idle[h.sessionName] = true
	h.record("pending interaction before idle timeout")
	h.reconcileTickWithIdle(it)

	if !h.env.sp.IsRunning(h.sessionName) {
		h.failf("pending interaction did not defer idle timeout stop for %q", h.sessionName)
	}
	if ds := h.env.dt.get(h.sessionID); ds != nil {
		h.failf("pending interaction idle timeout started drain: %+v", *ds)
	}
}

func TestSessionLifecycleChaosPendingInteractionCancelsExistingDrainBeforeIdleTimeout(t *testing.T) {
	h := newSessionChaosHarness(t, 20260435)
	h.createSessionIntent()
	h.assertCreatingIntent()
	h.reconcileTick()
	h.assertStarted()

	beginSessionDrain(h.mustBead(), h.env.sp, h.env.dt, "idle", h.env.clk, defaultDrainTimeout)
	if ds := h.env.dt.get(h.sessionID); ds == nil || ds.reason != "idle" {
		h.failf("expected idle drain before idle timeout, got %+v", ds)
	}
	h.env.sp.SetPendingInteraction(h.sessionName, &runtime.PendingInteraction{
		RequestID: "chaos-pending-idle-timeout-existing-drain",
		Kind:      "question",
		Prompt:    "chaos test",
	})
	it := newFakeIdleTracker()
	it.idle[h.sessionName] = true
	h.record("pending interaction with existing drain before idle timeout")
	h.reconcileTickWithIdle(it)

	if ds := h.env.dt.get(h.sessionID); ds != nil {
		h.failf("pending interaction idle timeout left existing drain active: %+v", *ds)
	}
	if !h.env.sp.IsRunning(h.sessionName) {
		h.failf("pending interaction idle timeout stopped runtime %q", h.sessionName)
	}
}

func TestSessionLifecycleChaosPendingInteractionDefersAwakeSetIdleSleep(t *testing.T) {
	h := newSessionChaosHarness(t, 20260431)
	h.env.cfg.Agents[0].SleepAfterIdle = "1s"
	h.createSessionIntent()
	h.assertCreatingIntent()
	h.reconcileTick()
	h.assertStarted()

	if err := h.env.store.SetMetadataBatch(h.sessionID, map[string]string{
		"detached_at":  h.env.clk.Now().Add(-2 * time.Second).UTC().Format(time.RFC3339),
		"sleep_intent": "idle-stop-pending",
	}); err != nil {
		h.failf("mark idle sleep candidate: %v", err)
	}
	h.env.sp.SetPendingInteraction(h.sessionName, &runtime.PendingInteraction{
		RequestID: "chaos-pending-awake-set-idle",
		Kind:      "question",
		Prompt:    "chaos test",
	})
	h.record("pending interaction before awake-set idle sleep")
	h.reconcileTick()

	if ds := h.env.dt.get(h.sessionID); ds != nil {
		h.failf("pending interaction awake-set idle sleep started drain: %+v", *ds)
	}
	if !h.env.sp.IsRunning(h.sessionName) {
		h.failf("pending interaction did not defer awake-set idle sleep for %q", h.sessionName)
	}
	b := h.mustBead()
	if got := b.Metadata["sleep_intent"]; got != "" {
		h.failf("pending interaction left sleep_intent=%q", got)
	}
}

func TestSessionLifecycleChaosPendingInteractionDefersReadyWaitIdleSleep(t *testing.T) {
	h := newSessionChaosHarness(t, 20260434)
	h.env.cfg.Agents[0].SleepAfterIdle = "1s"
	h.createSessionIntent()
	h.assertCreatingIntent()
	h.reconcileTick()
	h.assertStarted()

	if err := h.env.store.SetMetadataBatch(h.sessionID, map[string]string{
		"detached_at":  h.env.clk.Now().Add(-2 * time.Second).UTC().Format(time.RFC3339),
		"sleep_intent": "idle-stop-pending",
	}); err != nil {
		h.failf("mark ready-wait idle sleep candidate: %v", err)
	}
	h.env.sp.SetPendingInteraction(h.sessionName, &runtime.PendingInteraction{
		RequestID: "chaos-pending-ready-wait-idle",
		Kind:      "question",
		Prompt:    "chaos test",
	})
	h.record("pending interaction plus ready wait before idle sleep")
	h.reconcileTickWithReadyWait(map[string]bool{h.sessionID: true})

	if ds := h.env.dt.get(h.sessionID); ds != nil {
		h.failf("pending interaction plus ready wait started idle drain: %+v", *ds)
	}
	if !h.env.sp.IsRunning(h.sessionName) {
		h.failf("pending interaction plus ready wait did not keep %q running", h.sessionName)
	}
}

func TestSessionLifecycleChaosPendingInteractionRespectsWakeBlockers(t *testing.T) {
	cases := []struct {
		name string
		meta map[string]string
	}{
		{
			name: "wait-hold",
			meta: map[string]string{"wait_hold": "true"},
		},
		{
			name: "user-hold",
			meta: map[string]string{
				"held_until":   time.Date(2026, 3, 8, 12, 10, 0, 0, time.UTC).Format(time.RFC3339),
				"sleep_reason": "user-hold",
			},
		},
		{
			name: "quarantine",
			meta: map[string]string{
				"quarantined_until": time.Date(2026, 3, 8, 12, 10, 0, 0, time.UTC).Format(time.RFC3339),
				"sleep_reason":      "quarantine",
			},
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newSessionChaosHarness(t, 20260419+int64(i))
			h.createSessionIntent()
			h.assertCreatingIntent()
			h.reconcileTick()
			h.assertStarted()

			h.setDesired(false)
			if err := h.env.store.SetMetadataBatch(h.sessionID, tc.meta); err != nil {
				h.failf("set blocker metadata: %v", err)
			}
			h.env.sp.SetPendingInteraction(h.sessionName, &runtime.PendingInteraction{
				RequestID: "chaos-pending-blocked-" + tc.name,
				Kind:      "question",
				Prompt:    "chaos test",
			})
			h.record("pending interaction with blocker=%s", tc.name)
			h.reconcileTick()

			if ds := h.env.dt.get(h.sessionID); ds == nil {
				h.failf("pending interaction bypassed blocker %s; expected drain", tc.name)
			}
		})
	}
}

func TestSessionLifecycleChaosConfigSuspensionRemovesDesiredState(t *testing.T) {
	h := newSessionChaosHarness(t, 20260422)
	h.createSessionIntent()
	h.assertCreatingIntent()
	if !h.desired {
		h.failf("test setup expected desired session")
	}

	h.toggleConfigSuspended()

	if !h.env.cfg.Agents[0].Suspended {
		h.failf("toggleConfigSuspended did not suspend agent")
	}
	if h.desired {
		h.failf("suspended config left harness desired flag true")
	}
	if _, ok := h.env.desiredState[h.sessionName]; ok {
		h.failf("suspended config left desiredState entry for %q", h.sessionName)
	}
}

type sessionChaosOptions struct {
	seed       int64
	iterations int
	steps      int
}

const (
	maxSessionChaosIterations = 200
	maxSessionChaosSteps      = 500
)

func runSessionLifecycleChaos(t *testing.T, opts sessionChaosOptions) {
	t.Helper()
	opts = resolveSessionChaosOptions(t, opts)
	for iter := 0; iter < opts.iterations; iter++ {
		seed := opts.seed + int64(iter)
		t.Run(fmt.Sprintf("seed_%d", seed), func(t *testing.T) {
			h := newSessionChaosHarness(t, seed)
			h.createSessionIntent()
			h.assertCreatingIntent()
			h.reconcileTick()
			h.assertStarted()
			h.assertInvariants()

			for step := 0; step < opts.steps; step++ {
				h.step = step
				h.runRandomAction()
				h.assertInvariants()
			}
		})
	}
}

func resolveSessionChaosOptions(t *testing.T, opts sessionChaosOptions) sessionChaosOptions {
	t.Helper()
	seedFromEnv := false
	if opts.seed == 0 {
		opts.seed = 20260415
	}
	if raw := strings.TrimSpace(os.Getenv("GC_SESSION_CHAOS_SEED")); raw != "" {
		seed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			t.Fatalf("GC_SESSION_CHAOS_SEED=%q: %v", raw, err)
		}
		opts.seed = seed
		seedFromEnv = true
	}
	itersFromEnv := false
	if raw := strings.TrimSpace(os.Getenv("GC_SESSION_CHAOS_ITERS")); raw != "" {
		iters, err := strconv.Atoi(raw)
		if err != nil || iters <= 0 {
			t.Fatalf("GC_SESSION_CHAOS_ITERS=%q: want positive integer", raw)
		}
		if iters > maxSessionChaosIterations {
			t.Fatalf("GC_SESSION_CHAOS_ITERS=%q exceeds max %d", raw, maxSessionChaosIterations)
		}
		opts.iterations = iters
		itersFromEnv = true
	}
	stepsFromEnv := false
	if raw := strings.TrimSpace(os.Getenv("GC_SESSION_CHAOS_STEPS")); raw != "" {
		steps, err := strconv.Atoi(raw)
		if err != nil || steps <= 0 {
			t.Fatalf("GC_SESSION_CHAOS_STEPS=%q: want positive integer", raw)
		}
		if steps > maxSessionChaosSteps {
			t.Fatalf("GC_SESSION_CHAOS_STEPS=%q exceeds max %d", raw, maxSessionChaosSteps)
		}
		opts.steps = steps
		stepsFromEnv = true
	}
	nightly := os.Getenv("GC_SESSION_CHAOS_NIGHTLY") == "1"
	if nightly && !itersFromEnv && opts.iterations == 0 {
		opts.iterations = 20
	}
	if nightly && !stepsFromEnv && opts.steps == 0 {
		opts.steps = 80
	}
	if opts.iterations == 0 {
		if seedFromEnv && !itersFromEnv {
			opts.iterations = 1
		} else {
			opts.iterations = 6
		}
	}
	if opts.steps == 0 {
		opts.steps = 24
	}
	return opts
}

type sessionChaosHarness struct {
	t           *testing.T
	env         *reconcilerTestEnv
	manager     *sessionpkg.Manager
	rng         *rand.Rand
	seed        int64
	step        int
	template    string
	command     string
	sessionID   string
	sessionName string
	desired     bool
	trace       []string
}

func newSessionChaosHarness(t *testing.T, seed int64) *sessionChaosHarness {
	t.Helper()
	env := newReconcilerTestEnv()
	template := "chaos-worker"
	env.cfg = &config.City{
		Agents: []config.Agent{{
			Name:         template,
			StartCommand: "chaos-cmd",
		}},
	}
	return &sessionChaosHarness{
		t:        t,
		env:      env,
		manager:  sessionpkg.NewManager(env.store, env.sp),
		rng:      rand.New(rand.NewSource(seed)), //nolint:gosec // deterministic test chaos, not security-sensitive.
		seed:     seed,
		template: template,
		command:  "chaos-cmd",
	}
}

func (h *sessionChaosHarness) createSessionIntent() {
	if h.sessionID != "" {
		return
	}
	info, err := h.manager.CreateBeadOnly(
		h.template,
		"Chaos worker",
		h.command,
		"",
		"fake",
		"",
		nil,
		sessionpkg.ProviderResume{},
	)
	if err != nil {
		h.failf("CreateBeadOnly: %v", err)
	}
	h.sessionID = info.ID
	h.sessionName = info.SessionName
	h.setDesired(true)
	h.record("create-session id=%s name=%s", h.sessionID, h.sessionName)
}

func (h *sessionChaosHarness) templateParams() TemplateParams {
	return TemplateParams{
		Command:      h.command,
		SessionName:  h.sessionName,
		TemplateName: h.template,
		InstanceName: h.template,
		ResolvedProvider: &config.ResolvedProvider{
			Name:       "fake",
			Command:    h.command,
			PromptMode: "none",
		},
	}
}

func (h *sessionChaosHarness) setDesired(on bool) {
	if h.sessionName == "" {
		return
	}
	h.desired = on
	if on {
		h.env.desiredState[h.sessionName] = h.templateParams()
		return
	}
	delete(h.env.desiredState, h.sessionName)
}

func (h *sessionChaosHarness) reconcileTick() {
	sessions, err := loadSessionBeads(h.env.store)
	if err != nil {
		h.failf("loadSessionBeads: %v", err)
	}
	woken := h.env.reconcile(sessions)
	h.record("reconcile open=%d woken=%d", len(sessions), woken)
	h.assertPostReconcileInvariants()
}

func (h *sessionChaosHarness) reconcileTickWithIdle(it idleTracker) {
	sessions, err := loadSessionBeads(h.env.store)
	if err != nil {
		h.failf("loadSessionBeads: %v", err)
	}
	poolDesired := make(map[string]int)
	for _, tp := range h.env.desiredState {
		if tp.TemplateName != "" {
			poolDesired[tp.TemplateName]++
		}
	}
	woken := reconcileSessionBeads(
		context.Background(), sessions, h.env.desiredState, configuredSessionNames(h.env.cfg, "", h.env.store),
		h.env.cfg, h.env.sp, h.env.store, nil, nil, nil, h.env.dt, poolDesired, false, nil, "",
		it, h.env.clk, h.env.rec, 0, 0, &h.env.stdout, &h.env.stderr,
	)
	h.record("reconcile-with-idle open=%d woken=%d", len(sessions), woken)
	h.assertPostReconcileInvariants()
}

func (h *sessionChaosHarness) reconcileTickWithDrainOps() {
	sessions, err := loadSessionBeads(h.env.store)
	if err != nil {
		h.failf("loadSessionBeads: %v", err)
	}
	poolDesired := make(map[string]int)
	for _, tp := range h.env.desiredState {
		if tp.TemplateName != "" {
			poolDesired[tp.TemplateName]++
		}
	}
	woken := reconcileSessionBeads(
		context.Background(), sessions, h.env.desiredState, configuredSessionNames(h.env.cfg, "", h.env.store),
		h.env.cfg, h.env.sp, h.env.store, newDrainOps(h.env.sp), nil, nil, h.env.dt, poolDesired, false, nil, "",
		nil, h.env.clk, h.env.rec, 0, 0, &h.env.stdout, &h.env.stderr,
	)
	h.record("reconcile-with-drain-ops open=%d woken=%d", len(sessions), woken)
	h.assertPostReconcileInvariants()
}

func (h *sessionChaosHarness) reconcileTickWithReadyWait(readyWaitSet map[string]bool) {
	sessions, err := loadSessionBeads(h.env.store)
	if err != nil {
		h.failf("loadSessionBeads: %v", err)
	}
	poolDesired := make(map[string]int)
	for _, tp := range h.env.desiredState {
		if tp.TemplateName != "" {
			poolDesired[tp.TemplateName]++
		}
	}
	woken := reconcileSessionBeads(
		context.Background(), sessions, h.env.desiredState, configuredSessionNames(h.env.cfg, "", h.env.store),
		h.env.cfg, h.env.sp, h.env.store, nil, nil, readyWaitSet, h.env.dt, poolDesired, false, nil, "",
		nil, h.env.clk, h.env.rec, 0, 0, &h.env.stdout, &h.env.stderr,
	)
	h.record("reconcile-with-ready-wait open=%d woken=%d", len(sessions), woken)
	h.assertPostReconcileInvariants()
}

func (h *sessionChaosHarness) assertCreatingIntent() {
	b := h.mustBead()
	if got := b.Metadata["state"]; got != string(sessionpkg.StateCreating) {
		h.failf("new intent state = %q, want creating", got)
	}
	if got := b.Metadata["pending_create_claim"]; got != "true" {
		h.failf("new intent pending_create_claim = %q, want true", got)
	}
	if h.env.sp.IsRunning(h.sessionName) {
		h.failf("CreateBeadOnly started runtime for %q before reconcile", h.sessionName)
	}
}

func (h *sessionChaosHarness) assertStarted() {
	b := h.mustBead()
	if b.Status == "closed" {
		h.failf("session closed during first reconcile")
	}
	if got := b.Metadata["state"]; got != string(sessionpkg.StateActive) {
		h.failf("state after first reconcile = %q, want active", got)
	}
	if got := strings.TrimSpace(b.Metadata["pending_create_claim"]); got != "" {
		h.failf("pending_create_claim after first reconcile = %q, want cleared", got)
	}
	if !h.env.sp.IsRunning(h.sessionName) {
		h.failf("runtime %q not running after first reconcile", h.sessionName)
	}
	h.assertLastStartConfigMatches(b)
}

type sessionChaosAction struct {
	name string
	run  func()
}

func (h *sessionChaosHarness) runRandomAction() {
	actions := []sessionChaosAction{
		{name: "reconcile", run: h.reconcileTick},
		{name: "reconcile", run: h.reconcileTick},
		{name: "reconcile-with-drain-ops", run: h.reconcileTickWithDrainOps},
		{name: "advance-clock", run: h.advanceClock},
		{name: "provider-exit", run: h.injectProviderExit},
		{name: "request-restart", run: h.requestRestart},
		{name: "toggle-desired", run: h.toggleDesired},
		{name: "toggle-config-suspended", run: h.toggleConfigSuspended},
		{name: "toggle-pin", run: h.togglePin},
		{name: "toggle-attached", run: h.toggleAttached},
		{name: "toggle-pending-interaction", run: h.togglePendingInteraction},
		{name: "suspend", run: h.suspendSession},
		{name: "wake", run: h.wakeSession},
		{name: "archive-continuity", run: h.archiveContinuity},
		{name: "reactivate", run: h.reactivateSession},
		{name: "start-failure", run: h.injectStartFailure},
	}
	action := actions[h.rng.Intn(len(actions))]
	h.record("action=%s", action.name)
	action.run()
}

func (h *sessionChaosHarness) advanceClock() {
	durations := []time.Duration{
		time.Second,
		5 * time.Second,
		stabilityThreshold + time.Second,
		staleCreatingStateTimeout + time.Second,
		defaultDrainTimeout + time.Second,
		defaultQuarantineDuration + time.Second,
	}
	d := durations[h.rng.Intn(len(durations))]
	h.env.clk.Time = h.env.clk.Time.Add(d)
	h.record("clock += %s now=%s", d, h.env.clk.Now().UTC().Format(time.RFC3339))
}

func (h *sessionChaosHarness) injectProviderExit() {
	if h.sessionName == "" {
		return
	}
	if err := h.env.sp.Stop(h.sessionName); err != nil {
		h.failf("provider exit %s: %v", h.sessionName, err)
	}
	h.record("provider-exit name=%s", h.sessionName)
}

func (h *sessionChaosHarness) requestRestart() {
	if h.sessionName == "" || h.currentBeadClosed() {
		h.record("request-restart skipped")
		return
	}
	if err := setBeadRestartRequested(h.env.store, h.sessionName); err != nil {
		h.record("request-restart skipped: %v", err)
		return
	}
	h.record("request-restart name=%s", h.sessionName)
}

func (h *sessionChaosHarness) toggleDesired() {
	h.setDesired(!h.desired)
	h.record("desired=%t", h.desired)
}

func (h *sessionChaosHarness) toggleConfigSuspended() {
	if len(h.env.cfg.Agents) == 0 {
		return
	}
	h.env.cfg.Agents[0].Suspended = !h.env.cfg.Agents[0].Suspended
	h.setDesired(!h.env.cfg.Agents[0].Suspended)
	h.record("config-suspended=%t desired=%t", h.env.cfg.Agents[0].Suspended, h.desired)
}

func (h *sessionChaosHarness) togglePin() {
	b, ok := h.currentBead()
	if !ok || b.Status == "closed" {
		return
	}
	next := ""
	if b.Metadata["pin_awake"] != "true" {
		next = "true"
	}
	if err := h.env.store.SetMetadata(b.ID, "pin_awake", next); err != nil {
		h.failf("pin_awake=%q: %v", next, err)
	}
	h.record("pin_awake=%q", next)
}

func (h *sessionChaosHarness) toggleAttached() {
	if h.sessionName == "" {
		return
	}
	b, ok := h.currentBead()
	if !ok || b.Status == "closed" {
		return
	}
	next := !h.env.sp.IsAttached(h.sessionName)
	h.env.sp.SetAttached(h.sessionName, next)
	h.record("attached=%t", next)
}

func (h *sessionChaosHarness) togglePendingInteraction() {
	if h.sessionName == "" {
		return
	}
	b, ok := h.currentBead()
	if !ok || b.Status == "closed" {
		return
	}
	pending, err := h.env.sp.Pending(h.sessionName)
	if err != nil {
		h.failf("pending interaction lookup: %v", err)
	}
	if pending != nil {
		h.env.sp.SetPendingInteraction(h.sessionName, nil)
		h.record("pending-interaction=false")
		return
	}
	h.env.sp.SetPendingInteraction(h.sessionName, &runtime.PendingInteraction{
		RequestID: fmt.Sprintf("chaos-%d-%d", h.seed, h.step),
		Kind:      "question",
		Prompt:    "chaos test",
	})
	h.record("pending-interaction=true")
}

func (h *sessionChaosHarness) suspendSession() {
	b, ok := h.currentBead()
	if !ok || b.Status == "closed" {
		return
	}
	if strings.TrimSpace(b.Metadata["pending_create_claim"]) == "true" {
		h.record("suspend skipped: pending create")
		return
	}
	if err := h.manager.Suspend(b.ID); err != nil {
		h.record("suspend skipped: %v", err)
		return
	}
	h.record("suspend id=%s", b.ID)
}

func (h *sessionChaosHarness) wakeSession() {
	b, ok := h.currentBead()
	if !ok || b.Status == "closed" {
		return
	}
	if _, err := sessionpkg.WakeSession(h.env.store, b, h.env.clk.Now().UTC()); err != nil {
		h.record("wake skipped: %v", err)
		return
	}
	h.record("wake id=%s", b.ID)
}

func (h *sessionChaosHarness) archiveContinuity() {
	b, ok := h.currentBead()
	if !ok || b.Status == "closed" {
		return
	}
	if h.env.sp.IsRunning(h.sessionName) {
		if err := h.manager.Kill(b.ID); err != nil {
			h.record("archive kill skipped: %v", err)
		}
	}
	if err := h.manager.Archive(b.ID, "chaos-archive"); err != nil {
		h.record("archive skipped: %v", err)
		return
	}
	if h.rng.Intn(2) == 0 {
		if err := h.env.store.SetMetadata(b.ID, "continuity_eligible", "true"); err != nil {
			h.failf("mark continuity eligible: %v", err)
		}
		h.record("archive continuity=true")
		return
	}
	h.record("archive continuity=false")
}

func (h *sessionChaosHarness) reactivateSession() {
	b, ok := h.currentBead()
	if !ok || b.Status == "closed" {
		return
	}
	view := sessionpkg.ProjectLifecycle(sessionpkg.LifecycleInput{
		Status:   b.Status,
		Metadata: b.Metadata,
		Now:      h.env.clk.Now(),
	})
	switch view.BaseState {
	case sessionpkg.BaseStateArchived, sessionpkg.BaseStateQuarantined:
	default:
		h.record("reactivate skipped: state=%s", view.BaseState)
		return
	}
	if err := h.manager.Reactivate(b.ID); err != nil {
		h.record("reactivate skipped: %v", err)
		return
	}
	h.record("reactivate id=%s", b.ID)
}

func (h *sessionChaosHarness) injectStartFailure() {
	b, ok := h.currentBead()
	if !ok || b.Status == "closed" || h.sessionName == "" {
		return
	}
	if state, conflict := sessionpkg.LifecycleWakeConflictState(b.Status, b.Metadata); conflict {
		h.record("start-failure skipped: wake conflict state=%s", state)
		return
	}
	view := sessionpkg.ProjectLifecycle(sessionpkg.LifecycleInput{
		Status:   b.Status,
		Metadata: b.Metadata,
		Now:      h.env.clk.Now(),
	})
	switch view.BaseState {
	case sessionpkg.BaseStateActive, sessionpkg.BaseStateAsleep, sessionpkg.BaseStateStopped:
	default:
		h.record("start-failure skipped: state=%s", view.BaseState)
		return
	}
	h.setDesired(true)
	if len(h.env.cfg.Agents) > 0 {
		h.env.cfg.Agents[0].Suspended = false
	}
	_ = h.env.sp.Stop(h.sessionName)
	if err := h.env.store.SetMetadataBatch(b.ID, map[string]string{
		"state":                string(sessionpkg.StateActive),
		"pending_create_claim": "",
		"last_woke_at":         "",
		"pin_awake":            "true",
		"quarantined_until":    "",
		"restart_requested":    "",
		"wait_hold":            "",
	}); err != nil {
		h.failf("prepare start failure: %v", err)
	}
	startsBefore := h.countRuntimeCalls("Start")
	h.env.sp.StartErrors[h.sessionName] = errors.New("chaos start failure")
	h.record("start-failure armed")
	h.reconcileTick()
	delete(h.env.sp.StartErrors, h.sessionName)
	h.assertStartFailureRecorded(startsBefore)
	h.record("start-failure cleared")
}

func (h *sessionChaosHarness) assertStartFailureRecorded(startsBefore int) {
	startsAfter := h.countRuntimeCalls("Start")
	if startsAfter <= startsBefore {
		h.failf("start failure action did not attempt a runtime start")
	}
	b := h.mustBead()
	if h.env.sp.IsRunning(h.sessionName) {
		h.failf("runtime %q running after injected start failure", h.sessionName)
	}
	if b.Status == "closed" {
		h.failf("injected non-create start failure closed bead with reason %q", b.Metadata["close_reason"])
	}
	if got := strings.TrimSpace(b.Metadata["pending_create_claim"]); got != "" {
		h.failf("start failure left pending_create_claim=%q", got)
	}
	if got := strings.TrimSpace(b.Metadata["last_woke_at"]); got != "" {
		h.failf("start failure left last_woke_at=%q", got)
	}
	attempts, _ := strconv.Atoi(b.Metadata["wake_attempts"])
	if attempts <= 0 && strings.TrimSpace(b.Metadata["quarantined_until"]) == "" {
		h.failf("start failure did not record wake_attempts or quarantine")
	}
}

func (h *sessionChaosHarness) assertInvariants() {
	b, ok := h.currentBead()
	if !ok {
		return
	}
	if b.Metadata == nil {
		h.failf("session bead metadata is nil")
	}
	state := strings.TrimSpace(b.Metadata["state"])
	if b.Status != "closed" && state == "" {
		h.failf("open session has empty state")
	}
	if b.Status != "closed" && !knownChaosLifecycleState(state) {
		h.failf("open session has invalid lifecycle state %q", state)
	}
	if b.Status != "closed" && !sessionpkg.LifecycleIdentityReleased(b.Status, b.Metadata) && strings.TrimSpace(b.Metadata["session_name"]) == "" {
		h.failf("open session with retained identity has empty session_name")
	}

	runtimeName := strings.TrimSpace(b.Metadata["session_name"])
	if runtimeName == "" {
		runtimeName = h.sessionName
	} else {
		h.sessionName = runtimeName
	}
	running := runtimeName != "" && h.env.sp.IsRunning(runtimeName)
	if b.Status == "closed" && running {
		h.failf("closed session still has running runtime %q", runtimeName)
	}
	if running {
		switch sessionpkg.State(state) {
		case sessionpkg.StateActive, sessionpkg.StateAwake, sessionpkg.StateDraining:
			// These are the only lifecycle states that may own a live
			// runtime after an action has completed.
		default:
			h.failf("runtime %q running while state=%q", runtimeName, state)
		}
		if strings.TrimSpace(b.Metadata["pending_create_claim"]) != "" {
			h.failf("running session retained pending_create_claim=%q", b.Metadata["pending_create_claim"])
		}
		h.assertLastStartConfigMatches(b)
	}
	if state == string(sessionpkg.StateArchived) && b.Metadata["continuity_eligible"] != "true" && running {
		h.failf("continuity-ineligible archive still running runtime %q", runtimeName)
	}
	if b.Status != "closed" && strings.TrimSpace(b.Metadata["pending_create_claim"]) == "true" && state != string(sessionpkg.StateCreating) {
		h.failf("pending_create_claim=true with state=%q", state)
	}
}

func (h *sessionChaosHarness) assertPostReconcileInvariants() {
	b, ok := h.currentBead()
	if !ok || b.Status == "closed" {
		return
	}
	runtimeName := strings.TrimSpace(b.Metadata["session_name"])
	if runtimeName == "" {
		runtimeName = h.sessionName
	}
	if runtimeName == "" {
		return
	}
	if pendingInteractionKeepsAwake(b, h.env.sp, runtimeName, h.env.clk) {
		if ds := h.env.dt.get(b.ID); ds != nil {
			if !drainReasonCancelable(ds.reason) {
				return
			}
			h.failf("pending interaction coexists with active drain after reconcile: %+v", *ds)
		}
	}
}

func knownChaosLifecycleState(state string) bool {
	switch sessionpkg.BaseState(state) {
	case sessionpkg.BaseStateNone,
		sessionpkg.BaseStateCreating,
		sessionpkg.BaseStateActive,
		sessionpkg.BaseStateAsleep,
		sessionpkg.BaseStateSuspended,
		sessionpkg.BaseStateDraining,
		sessionpkg.BaseStateDrained,
		sessionpkg.BaseStateArchived,
		sessionpkg.BaseStateOrphaned,
		sessionpkg.BaseStateClosed,
		sessionpkg.BaseStateClosing,
		sessionpkg.BaseStateQuarantined,
		sessionpkg.BaseStateStopped:
		return true
	default:
		return state == string(sessionpkg.StateAwake)
	}
}

func (h *sessionChaosHarness) assertLastStartConfigMatches(b beads.Bead) {
	cfg := h.env.sp.LastStartConfig(h.sessionName)
	if cfg == nil {
		h.failf("runtime %q is running without a recorded Start config", h.sessionName)
	}
	if cfg.Command == "" {
		h.failf("runtime %q started with empty command", h.sessionName)
	}
	env := cfg.Env
	if env["GC_SESSION_ID"] != b.ID {
		h.failf("GC_SESSION_ID = %q, want %q", env["GC_SESSION_ID"], b.ID)
	}
	if env["GC_SESSION_NAME"] != h.sessionName {
		h.failf("GC_SESSION_NAME = %q, want %q", env["GC_SESSION_NAME"], h.sessionName)
	}
	if env["GC_TEMPLATE"] != h.template {
		h.failf("GC_TEMPLATE = %q, want %q", env["GC_TEMPLATE"], h.template)
	}
	if env["GC_INSTANCE_TOKEN"] == "" {
		h.failf("GC_INSTANCE_TOKEN is empty")
	}
	if env["GC_RUNTIME_EPOCH"] == "" {
		h.failf("GC_RUNTIME_EPOCH is empty")
	}
	if env["GC_CONTINUATION_EPOCH"] == "" {
		h.failf("GC_CONTINUATION_EPOCH is empty")
	}
}

func (h *sessionChaosHarness) currentBead() (beads.Bead, bool) {
	if h.sessionID == "" {
		return beads.Bead{}, false
	}
	b, err := h.env.store.Get(h.sessionID)
	if err != nil {
		h.failf("session bead %q not found: %v", h.sessionID, err)
	}
	return b, true
}

func (h *sessionChaosHarness) currentBeadClosed() bool {
	b, ok := h.currentBead()
	return !ok || b.Status == "closed"
}

func (h *sessionChaosHarness) mustBead() beads.Bead {
	h.t.Helper()
	b, ok := h.currentBead()
	if !ok {
		h.failf("session bead %q not found", h.sessionID)
	}
	return b
}

func (h *sessionChaosHarness) record(format string, args ...any) {
	h.trace = append(h.trace, fmt.Sprintf("%02d: %s", h.step, fmt.Sprintf(format, args...)))
}

func (h *sessionChaosHarness) countRuntimeCalls(method string) int {
	count := 0
	for _, call := range h.env.sp.Calls {
		if call.Method == method && call.Name == h.sessionName {
			count++
		}
	}
	return count
}

func (h *sessionChaosHarness) failf(format string, args ...any) {
	h.t.Helper()
	h.t.Fatalf("%s\n%s", fmt.Sprintf(format, args...), h.debugDump())
}

func (h *sessionChaosHarness) debugDump() string {
	var sections []string
	sections = append(sections,
		fmt.Sprintf("seed=%d step=%d now=%s desired=%t", h.seed, h.step, h.env.clk.Now().UTC().Format(time.RFC3339), h.desired),
		"trace:\n"+strings.Join(h.trace, "\n"),
	)
	if h.sessionID != "" {
		if b, err := h.env.store.Get(h.sessionID); err == nil {
			sections = append(sections, "session:\n"+jsonForDebug(b))
		}
	}
	sections = append(sections,
		"provider calls:\n"+formatRuntimeCalls(h.env.sp.Calls),
		"stdout:\n"+h.env.stdout.String(),
		"stderr:\n"+h.env.stderr.String(),
	)
	return strings.Join(sections, "\n\n")
}

func jsonForDebug(v any) string {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("%#v", v)
	}
	return string(data)
}

func formatRuntimeCalls(calls []runtime.Call) string {
	if len(calls) == 0 {
		return "(none)"
	}
	const maxCalls = 80
	start := 0
	if len(calls) > maxCalls {
		start = len(calls) - maxCalls
	}
	lines := make([]string, 0, len(calls)-start+1)
	if start > 0 {
		lines = append(lines, fmt.Sprintf("... %d earlier calls omitted", start))
	}
	for i := start; i < len(calls); i++ {
		call := calls[i]
		detail := call.Method
		if call.Name != "" {
			detail += " " + call.Name
		}
		if call.Key != "" {
			detail += " " + call.Key + "=" + call.Value
		}
		if call.Method == "Start" {
			detail += " command=" + strconv.Quote(call.Config.Command)
		}
		lines = append(lines, fmt.Sprintf("%03d %s", i, detail))
	}
	return strings.Join(lines, "\n")
}
