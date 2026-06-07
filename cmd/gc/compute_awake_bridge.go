package main

import (
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// buildAwakeInputFromReconciler constructs AwakeInput from the reconciler's
// existing data. Runtime liveness is populated from the already-computed
// wakeTargets; attachment and pending interactions come from provider
// capability probes.
func buildAwakeInputFromReconciler(
	cfg *config.City,
	cityPath string,
	sessionBeads []beads.Bead,
	poolDesired map[string]int,
	namedSessionDemand map[string]bool,
	workSet map[string]bool,
	readyWaitSet map[string]bool,
	assignedWorkBeads []beads.Bead,
	wakeTargets []wakeTarget,
	sp runtime.Provider,
	clk time.Time,
) AwakeInput {
	input := AwakeInput{
		ScaleCheckCounts:   poolDesired,
		NamedSessionDemand: cloneBoolMap(namedSessionDemand),
		WorkSet:            workSet,
		ReadyWaitSet:       readyWaitSet,
		RunningSessions:    make(map[string]bool),
		AttachedSessions:   make(map[string]bool),
		PendingSessions:    make(map[string]bool),
		ChatIdleTimeout:    cfg.ChatSessions.IdleTimeoutDuration(),
		ManualGracePeriod:  cfg.ChatSessions.GracePeriodDuration(),
		Now:                clk,
	}

	// Agents. Load runtime suspension state once against the in-scope
	// city path so suspension resolves against the controlled city
	// rather than the process cwd.
	suspState, _ := loadSuspensionState(fsys.OSFS{}, cityPath)
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		agent := AwakeAgent{
			QualifiedName:     a.QualifiedName(),
			Suspended:         isAgentEffectivelySuspendedWith(cfg, a, suspState),
			SleepAfterIdle:    parseSleepDuration(a.SleepAfterIdle),
			MinActiveSessions: a.EffectiveMinActiveSessions(),
		}
		if len(a.DependsOn) > 0 {
			agent.DependsOn = a.DependsOn
		}
		input.Agents = append(input.Agents, agent)
	}

	// Named sessions
	cityName := config.EffectiveCityName(cfg, "")
	for i := range cfg.NamedSessions {
		ns := &cfg.NamedSessions[i]
		identity := ns.QualifiedName()
		input.NamedSessions = append(input.NamedSessions, AwakeNamedSession{
			Identity:    identity,
			Template:    ns.TemplateQualifiedName(),
			Mode:        ns.Mode,
			RuntimeName: config.NamedSessionRuntimeName(cityName, cfg.Workspace, identity),
		})
	}

	// Work beads
	for _, wb := range assignedWorkBeads {
		a := strings.TrimSpace(wb.Assignee)
		if a != "" && (wb.Status == "open" || wb.Status == "in_progress") {
			// assignedWorkBeads is the reconciler's actionable snapshot:
			// in-progress work plus open work that has already passed readiness
			// and blocker filtering.
			input.WorkBeads = append(input.WorkBeads, AwakeWorkBead{
				ID: wb.ID, Assignee: a, Status: wb.Status, Ready: wb.Status == "open",
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
			ID:                     b.ID,
			SessionName:            name,
			Template:               b.Metadata["template"],
			State:                  string(lifecycle.CompatState),
			SleepReason:            b.Metadata["sleep_reason"],
			ManualSession:          isManualSessionBead(*b),
			PendingCreate:          lifecycle.HasWakeCause(session.WakeCausePendingCreate),
			ExplicitWake:           lifecycle.HasWakeCause(session.WakeCauseExplicit),
			DependencyOnly:         b.Metadata["dependency_only"] == "true",
			NamedIdentity:          lifecycle.NamedIdentity,
			ConfiguredNamedSession: isNamedSessionBead(*b),
			Pinned:                 lifecycle.HasWakeCause(session.WakeCausePinned),
			Drained:                lifecycle.BaseState == session.BaseStateDrained,
			WaitHold:               b.Metadata["wait_hold"] == "true",
			RestartRequested:       strings.TrimSpace(b.Metadata["restart_requested"]) == "true",
			ContinuationResetPending: strings.TrimSpace(b.Metadata["continuation_reset_pending"]) == "true" &&
				strings.TrimSpace(b.Metadata[session.ResetCommittedAtKey]) != "",
		}
		bead.HeldUntil = lifecycle.HeldUntil
		bead.QuarantinedUntil = lifecycle.QuarantinedUntil
		bead.CreatedAt = b.CreatedAt
		if t, err := time.Parse(time.RFC3339, b.Metadata["detached_at"]); err == nil && !t.IsZero() {
			bead.IdleSince = t
		}
		input.SessionBeads = append(input.SessionBeads, bead)
	}

	// Preserve the reconciler's existing wake continuity for already-materialized
	// on-demand named sessions: when work_query matched the backing template and
	// the canonical bead still exists, carry an explicit named-session work-query
	// signal rather than waking ordinary siblings from the generic WorkSet path.
	for _, ns := range input.NamedSessions {
		if ns.Mode != "on_demand" || !input.WorkSet[ns.Template] {
			continue
		}
		if resolveNamedSessionBeadName(input.SessionBeads, ns) == "" {
			continue
		}
		if input.NamedSessionWorkQ == nil {
			input.NamedSessionWorkQ = make(map[string]bool)
		}
		input.NamedSessionWorkQ[ns.Identity] = true
	}

	// Runtime liveness comes from wakeTargets. Attachment is probed only when
	// it can affect the awake decision; the common active desired-session path
	// is already awake and has no idle reference to suppress.
	for _, target := range wakeTargets {
		name := strings.TrimSpace(target.session.Metadata["session_name"])
		if name == "" {
			continue
		}
		if target.alive {
			input.RunningSessions[name] = true
		}
		if shouldProbeAttachmentForAwakeInput(target, cfg, poolDesired) {
			if attached, err := workerSessionTargetAttachedWithConfig("", nil, sp, nil, name); err == nil && attached {
				input.AttachedSessions[name] = true
			}
		}
		if pendingInteractionReady(sp, name) {
			input.PendingSessions[name] = true
		}
	}

	return input
}

func shouldProbeAttachmentForAwakeInput(target wakeTarget, cfg *config.City, poolDesired map[string]int) bool {
	if target.session == nil {
		return false
	}
	if !target.alive {
		return false
	}
	state := target.session.Metadata["state"]
	if state != string(session.StateActive) && state != string(session.StateAwake) {
		return true
	}
	if target.session.Metadata["detached_at"] != "" {
		return true
	}
	template := normalizedSessionTemplate(*target.session, cfg)
	if template == "" {
		template = target.session.Metadata["template"]
	}
	if template != "" && poolDesired[template] > 0 {
		return false
	}
	return true
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
			case "explicit-wake":
				reasons = []WakeReason{WakeConfig}
			case "attached":
				reasons = []WakeReason{WakeAttached}
			case "pending":
				reasons = []WakeReason{WakePending}
			case "pin":
				reasons = []WakeReason{WakePin}
			case "wait-ready":
				reasons = []WakeReason{WakeWait}
			case "assigned-work", "named-demand", "work-query":
				reasons = []WakeReason{WakeWork}
			case "min-active":
				reasons = []WakeReason{WakeConfig}
			default:
				reasons = []WakeReason{WakeConfig}
			}
		}
		evals[bead.ID] = wakeEvaluation{
			Reasons:          reasons,
			Reason:           d.Reason,
			ConfigSuppressed: d.Reason == "idle-sleep",
			HasAssignedWork:  d.HasAssignedWork,
		}
	}
	return evals
}

func cloneBoolMap(source map[string]bool) map[string]bool {
	if source == nil {
		return nil
	}
	out := make(map[string]bool, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
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
