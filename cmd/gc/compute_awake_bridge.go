package main

import (
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// buildAwakeInputFromReconciler constructs AwakeInput from the reconciler's
// existing data. Runtime liveness is populated from the already-computed
// wakeTargets; attachment and pending interactions come from provider
// capability probes.
func buildAwakeInputFromReconciler(
	cfg *config.City,
	sessionBeads []beads.Bead,
	poolDesired map[string]int,
	workSet map[string]bool,
	readyWaitSet map[string]bool,
	assignedWorkBeads []beads.Bead,
	wakeTargets []wakeTarget,
	sp runtime.Provider,
	clk time.Time,
) AwakeInput {
	input := AwakeInput{
		ScaleCheckCounts: poolDesired,
		WorkSet:          workSet,
		ReadyWaitSet:     readyWaitSet,
		RunningSessions:  make(map[string]bool),
		AttachedSessions: make(map[string]bool),
		PendingSessions:  make(map[string]bool),
		ChatIdleTimeout:  cfg.ChatSessions.IdleTimeoutDuration(),
		Now:              clk,
	}

	// Agents
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		agent := AwakeAgent{
			QualifiedName:  a.QualifiedName(),
			Suspended:      isAgentEffectivelySuspended(cfg, a),
			SleepAfterIdle: parseSleepDuration(a.SleepAfterIdle),
		}
		if len(a.DependsOn) > 0 {
			agent.DependsOn = a.DependsOn
		}
		input.Agents = append(input.Agents, agent)
	}

	// Named sessions
	for i := range cfg.NamedSessions {
		ns := &cfg.NamedSessions[i]
		input.NamedSessions = append(input.NamedSessions, AwakeNamedSession{
			Identity: ns.QualifiedName(),
			Template: ns.TemplateQualifiedName(),
			Mode:     ns.Mode,
		})
	}

	// Work beads
	for _, wb := range assignedWorkBeads {
		a := strings.TrimSpace(wb.Assignee)
		if a != "" && (wb.Status == "open" || wb.Status == "in_progress") {
			input.WorkBeads = append(input.WorkBeads, AwakeWorkBead{
				ID: wb.ID, Assignee: a, Status: wb.Status,
			})
		}
	}

	// Session beads
	for i := range sessionBeads {
		b := &sessionBeads[i]
		if b.Status == "closed" {
			continue
		}
		name := strings.TrimSpace(b.Metadata["session_name"])
		if name == "" {
			continue
		}
		lifecycle := session.ProjectLifecycle(session.LifecycleInput{
			Status:   b.Status,
			Metadata: b.Metadata,
			Now:      clk,
		})
		bead := AwakeSessionBead{
			ID:             b.ID,
			SessionName:    name,
			Template:       b.Metadata["template"],
			State:          string(lifecycle.CompatState),
			SleepReason:    b.Metadata["sleep_reason"],
			ManualSession:  isManualSessionBead(*b),
			PendingCreate:  lifecycle.HasWakeCause(session.WakeCausePendingCreate),
			DependencyOnly: b.Metadata["dependency_only"] == "true",
			NamedIdentity:  lifecycle.NamedIdentity,
			Pinned:         lifecycle.HasWakeCause(session.WakeCausePinned),
			Drained:        lifecycle.BaseState == session.BaseStateDrained,
			WaitHold:       b.Metadata["wait_hold"] == "true",
		}
		bead.HeldUntil = lifecycle.HeldUntil
		bead.QuarantinedUntil = lifecycle.QuarantinedUntil
		if t, err := time.Parse(time.RFC3339, b.Metadata["detached_at"]); err == nil && !t.IsZero() {
			bead.IdleSince = t
		}
		input.SessionBeads = append(input.SessionBeads, bead)
	}

	// Runtime state from wakeTargets (already computed, no extra tmux calls)
	for _, target := range wakeTargets {
		name := strings.TrimSpace(target.session.Metadata["session_name"])
		if name == "" {
			continue
		}
		if target.alive {
			input.RunningSessions[name] = true
		}
		if attached, err := workerSessionTargetAttachedWithConfig("", nil, sp, nil, name); err == nil && attached {
			input.AttachedSessions[name] = true
		}
		if pendingInteractionReady(sp, name) {
			input.PendingSessions[name] = true
		}
	}

	return input
}

// awakeSetToWakeEvals converts ComputeAwakeSet output to wakeEvaluation map
// for compatibility with advanceSessionDrainsWithSessions.
func awakeSetToWakeEvals(decisions map[string]AwakeDecision, sessionBeads []AwakeSessionBead) map[string]wakeEvaluation {
	evals := make(map[string]wakeEvaluation, len(decisions))
	for _, bead := range sessionBeads {
		d, ok := decisions[bead.SessionName]
		if !ok {
			continue
		}
		var reasons []WakeReason
		if d.ShouldWake {
			switch d.Reason {
			case "pending-create":
				reasons = []WakeReason{WakeCreate}
			case "attached":
				reasons = []WakeReason{WakeAttached}
			case "pending":
				reasons = []WakeReason{WakePending}
			case "pin":
				reasons = []WakeReason{WakePin}
			case "wait-ready":
				reasons = []WakeReason{WakeWait}
			case "assigned-work", "work-query":
				reasons = []WakeReason{WakeWork}
			default:
				reasons = []WakeReason{WakeConfig}
			}
		}
		evals[bead.ID] = wakeEvaluation{
			Reasons:          reasons,
			ConfigSuppressed: d.Reason == "idle-sleep",
		}
	}
	return evals
}

func parseSleepDuration(s string) time.Duration {
	if s == "" || s == "off" {
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0
	}
	return d
}
