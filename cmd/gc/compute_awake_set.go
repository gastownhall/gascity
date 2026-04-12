package main

import (
	"strings"
	"time"
)

// defaultOnDemandIdleTimeout is the fallback idle timeout for on-demand
// named sessions that don't configure an explicit idle_timeout. Without
// this, on-demand sessions kept alive by the "on-demand:running" override
// would stay awake indefinitely. 5 minutes is long enough to handle a
// conversation turn, short enough to not waste resources.
const defaultOnDemandIdleTimeout = 5 * time.Minute

// AwakeInput contains all pre-computed state needed to decide which sessions
// should be awake. All external I/O (shell commands, tmux checks, store
// queries) happens before this function is called.
type AwakeInput struct {
	Agents           []AwakeAgent
	NamedSessions    []AwakeNamedSession
	SessionBeads     []AwakeSessionBead
	WorkBeads        []AwakeWorkBead
	ScaleCheckCounts map[string]int  // agent template → desired count
	WorkSet          map[string]bool // agent template → work_query found pending work
	RunningSessions  map[string]bool // session name → tmux exists
	AttachedSessions map[string]bool // session name → user attached
	PendingSessions  map[string]bool // session name → pending interaction
	ReadyWaitSet     map[string]bool // session bead ID → durable wait is ready
	ChatIdleTimeout  time.Duration   // global idle timeout for manual/chat sessions (0 = disabled)
	Now              time.Time
}

// AwakeAgent represents an [[agent]] config entry.
type AwakeAgent struct {
	QualifiedName  string   // e.g. "hello-world/polecat"
	DependsOn      []string // template names this agent depends on
	Suspended      bool
	SleepAfterIdle time.Duration // 0 = disabled
}

// AwakeNamedSession represents a [[named_session]] config entry.
type AwakeNamedSession struct {
	Identity string // qualified name, e.g. "hello-world/refinery"
	Template string // agent template name
	Mode     string // "always" or "on_demand"
}

// AwakeSessionBead represents an open session bead from the store.
type AwakeSessionBead struct {
	ID               string
	SessionName      string
	Template         string
	State            string // "creating", "active", "asleep", "drained", "closed"
	SleepReason      string
	ManualSession    bool
	PendingCreate    bool      // controller claimed this bead for initial start
	DependencyOnly   bool      // only wakeable via dependency gate
	NamedIdentity    string    // non-empty for named session beads
	Pinned           bool      // pin_awake durable wake reason
	Drained          bool      // state=="drained" or sleep_reason=="drained"
	WaitHold         bool      // user-issued gc wait in progress
	HeldUntil        time.Time // zero = not held
	QuarantinedUntil time.Time // zero = not quarantined
	IdleSince        time.Time // zero = unknown/not idle
}

// AwakeWorkBead represents a work bead with an assignee.
type AwakeWorkBead struct {
	ID       string
	Assignee string
	Status   string // "open", "in_progress"
}

// AwakeDecision is the output for a single session.
type AwakeDecision struct {
	ShouldWake bool
	Reason     string // human-readable reason for debugging
}

// ComputeAwakeSet determines which sessions should be awake.
//
// Pure function. Algorithm:
//  1. Build desired set from config + demand signals
//  2. Any session in desired set should wake
//  3. Attached/pending/ready-wait override (wake even if not desired)
//  4. Idle sleep suppression
//  5. Hold + quarantine suppression (overrides everything)
//
// Dependency ordering is NOT enforced here — the reconciler's
// executePlannedStarts handles it via wave-based starts.
func ComputeAwakeSet(input AwakeInput) map[string]AwakeDecision {
	agentsByName := make(map[string]AwakeAgent, len(input.Agents))
	for _, a := range input.Agents {
		agentsByName[a.QualifiedName] = a
	}

	// Step 1: Build desired set.
	// Drained and dependency_only beads are excluded from demand-driven wake.
	desired := make(map[string]string) // sessionName → reason

	// Newly created beads that still carry a controller create claim must be
	// launched at least once, even if the work signal that materialized them
	// is no longer visible on the very next tick.
	for _, bead := range input.SessionBeads {
		if bead.Drained || !bead.PendingCreate {
			continue
		}
		desired[bead.SessionName] = "pending-create"
	}

	// Named sessions
	for _, ns := range input.NamedSessions {
		if agent, ok := agentsByName[ns.Identity]; ok && agent.Suspended {
			continue
		}
		switch ns.Mode {
		case "always":
			if sn := findNamedSessionName(input.SessionBeads, ns.Identity); sn != "" {
				bead := findBeadBySessionName(input.SessionBeads, sn)
				if bead != nil && !bead.Drained && !bead.DependencyOnly {
					desired[sn] = "named-always"
				}
			} else {
				desired[ns.Identity] = "named-always"
			}
		case "on_demand":
			// On-demand named sessions materialize from direct targeting,
			// direct concrete ownership, dependencies, binding continuity,
			// and pinning. Generic scale_check demand belongs to ephemeral
			// capacity, not named identity materialization.
		}
	}

	// Agent templates (scaled)
	for template, count := range input.ScaleCheckCounts {
		if count <= 0 {
			continue
		}
		agent, ok := agentsByName[template]
		if !ok || agent.Suspended {
			continue
		}
		active := collectActiveBeads(input.SessionBeads, template)
		for i, bead := range active {
			if i >= count {
				break
			}
			desired[bead.SessionName] = "scaled:demand"
		}
		creating := collectCreatingBeads(input.SessionBeads, template)
		filled := len(active)
		for _, bead := range creating {
			if filled >= count {
				break
			}
			desired[bead.SessionName] = "scaled:creating"
			filled++
		}
	}

	// WorkSet: defense-in-depth wake signal from work_query.
	// When work_query sees pending work but ScaleCheckCounts hasn't caught up
	// (count is 0 or absent), wake exactly one session to handle it. This
	// avoids thundering herd — scale_check will catch up on the next tick.
	for template, hasWork := range input.WorkSet {
		if !hasWork {
			continue
		}
		if input.ScaleCheckCounts[template] > 0 {
			continue // ScaleCheck already covers this template
		}
		agent, ok := agentsByName[template]
		if !ok || agent.Suspended {
			continue
		}
		if isNamedSessionTemplate(input.NamedSessions, template) {
			continue // named sessions are handled in the named-session pass
		}
		// collectActiveBeads already excludes DependencyOnly and Drained
		if active := collectActiveBeads(input.SessionBeads, template); len(active) > 0 {
			desired[active[0].SessionName] = "work-query"
			continue
		}
		if creating := collectCreatingBeads(input.SessionBeads, template); len(creating) > 0 {
			desired[creating[0].SessionName] = "work-query"
		}
	}

	// Manual sessions
	for _, bead := range input.SessionBeads {
		if !bead.ManualSession || bead.State == "closed" || bead.Drained {
			continue
		}
		if _, ok := agentsByName[bead.Template]; ok {
			desired[bead.SessionName] = "manual"
		}
	}

	// Sessions with assigned work — a session that has open or in_progress
	// work assigned to it must stay awake. Compatibility-only readers still
	// accept current session_name and exact configured named identity tokens,
	// but normal targeting surfaces write the concrete bead ID.
	for _, bead := range input.SessionBeads {
		if bead.State == "closed" || bead.Drained {
			continue
		}
		if _, already := desired[bead.SessionName]; already {
			continue
		}
		if agent, ok := agentsByName[bead.Template]; ok && agent.Suspended {
			continue
		}
		for _, wb := range input.WorkBeads {
			assignee := strings.TrimSpace(wb.Assignee)
			if assignee == "" || (wb.Status != "open" && wb.Status != "in_progress") {
				continue
			}
			if assignee == bead.ID || assignee == bead.SessionName || (bead.NamedIdentity != "" && assignee == bead.NamedIdentity) {
				desired[bead.SessionName] = "assigned-work"
				break
			}
		}
	}

	// Step 2-3: Decide awake
	result := make(map[string]AwakeDecision)

	for _, bead := range input.SessionBeads {
		name := bead.SessionName
		decision := AwakeDecision{}

		// Desired set (demand-driven wake). wait_hold suppresses normal
		// demand-driven wake so a session intentionally parked on human
		// input stays asleep until either its durable wait becomes ready
		// or it still needs its initial launch.
		if reason, inDesired := desired[name]; inDesired {
			if !bead.WaitHold || bead.PendingCreate {
				decision.ShouldWake = true
				decision.Reason = reason
			}
		}

		// Attached override — even drained beads wake if user is attached
		if input.AttachedSessions[name] && !bead.WaitHold {
			decision.ShouldWake = true
			decision.Reason = "attached"
		}

		// Pending interaction override — even drained beads wake
		if input.PendingSessions[name] && !bead.WaitHold {
			decision.ShouldWake = true
			decision.Reason = "pending"
		}

		// Ready wait — durable wait deadline passed, resume session
		if input.ReadyWaitSet[bead.ID] {
			decision.ShouldWake = true
			decision.Reason = "wait-ready"
		}

		// On-demand running override — on-demand sessions that are
		// currently running stay awake even when demand drops to zero.
		// They drain via idle_timeout, not demand absence. This
		// supports message-driven wake: a message starts the session,
		// it stays alive handling it, then idles until timeout.
		// Drain-ack agents are unaffected — they manage their own
		// lifecycle by calling drain-ack before this check matters.
		if !decision.ShouldWake && !bead.Drained && !bead.WaitHold &&
			bead.SleepReason != "idle-timeout" {
			if input.RunningSessions[name] && isOnDemandSession(input.NamedSessions, bead) {
				decision.ShouldWake = true
				decision.Reason = "on-demand:running"
			}
		}

		// Durable pin override — wakes and keeps the session awake while
		// still respecting hard blockers applied below.
		pinBlockedByState := bead.State == "suspended" || bead.State == "closed"
		if !decision.ShouldWake && bead.Pinned && !pinBlockedByState && !bead.DependencyOnly && !bead.Drained && !bead.WaitHold {
			if agent, ok := agentsByName[bead.Template]; ok && !agent.Suspended {
				decision.ShouldWake = true
				decision.Reason = "pin"
			}
		}

		// Idle sleep: desired sessions idle too long should sleep.
		// Attached, pinned, and mode=always named sessions are exempt.
		if decision.ShouldWake && !input.AttachedSessions[name] && !bead.Pinned && !bead.IdleSince.IsZero() &&
			!isAlwaysNamedSession(input.NamedSessions, bead) {
			agent, hasAgent := agentsByName[bead.Template]
			var idleTimeout time.Duration
			switch {
			case bead.ManualSession && input.ChatIdleTimeout > 0:
				idleTimeout = input.ChatIdleTimeout
			case hasAgent && agent.SleepAfterIdle > 0:
				idleTimeout = agent.SleepAfterIdle
			case isOnDemandSession(input.NamedSessions, bead):
				idleTimeout = defaultOnDemandIdleTimeout
			}
			if idleTimeout > 0 && input.Now.Sub(bead.IdleSince) >= idleTimeout {
				decision.ShouldWake = false
				decision.Reason = "idle-sleep"
			}
		}

		// Hold suppression — overrides everything
		if !bead.HeldUntil.IsZero() && input.Now.Before(bead.HeldUntil) {
			decision.ShouldWake = false
			decision.Reason = "held"
		}

		// Quarantine suppression — overrides everything
		if !bead.QuarantinedUntil.IsZero() && input.Now.Before(bead.QuarantinedUntil) {
			decision.ShouldWake = false
			decision.Reason = "quarantined"
		}

		// NOTE: Dependency ordering is NOT enforced here. The reconciler's
		// executePlannedStarts handles dependency-aware wave ordering via
		// allDependenciesAliveForTemplate at wave boundaries. Applying
		// the gate here would prevent candidates from reaching the start
		// list, breaking wave-based starts (where dep starts in wave 0
		// and dependent starts in wave 1).

		result[name] = decision
	}

	return result
}

func findNamedSessionName(beads []AwakeSessionBead, identity string) string {
	for _, b := range beads {
		if b.NamedIdentity == identity {
			return b.SessionName
		}
	}
	return ""
}

func findBeadBySessionName(beads []AwakeSessionBead, name string) *AwakeSessionBead {
	for i := range beads {
		if beads[i].SessionName == name {
			return &beads[i]
		}
	}
	return nil
}

func isNamedSessionTemplate(named []AwakeNamedSession, template string) bool {
	for _, ns := range named {
		if ns.Template == template {
			return true
		}
	}
	return false
}

func collectActiveBeads(beads []AwakeSessionBead, template string) []AwakeSessionBead {
	var result []AwakeSessionBead
	for _, b := range beads {
		if b.Template == template && b.State == "active" && b.NamedIdentity == "" && !b.ManualSession && !b.Drained && !b.DependencyOnly {
			result = append(result, b)
		}
	}
	return result
}

func isOnDemandSession(named []AwakeNamedSession, bead AwakeSessionBead) bool {
	if bead.NamedIdentity == "" {
		return false
	}
	for _, ns := range named {
		if ns.Identity == bead.NamedIdentity && ns.Mode == "on_demand" {
			return true
		}
	}
	return false
}

func isAlwaysNamedSession(named []AwakeNamedSession, bead AwakeSessionBead) bool {
	if bead.NamedIdentity == "" {
		return false
	}
	for _, ns := range named {
		if ns.Identity == bead.NamedIdentity && ns.Mode == "always" {
			return true
		}
	}
	return false
}

func collectCreatingBeads(beads []AwakeSessionBead, template string) []AwakeSessionBead {
	var result []AwakeSessionBead
	for _, b := range beads {
		if b.Template == template && b.State == "creating" && b.NamedIdentity == "" && !b.ManualSession && !b.Drained && !b.DependencyOnly {
			result = append(result, b)
		}
	}
	return result
}
