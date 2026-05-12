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
	Agents             []AwakeAgent
	NamedSessions      []AwakeNamedSession
	SessionBeads       []AwakeSessionBead
	WorkBeads          []AwakeWorkBead
	ScaleCheckCounts   map[string]int  // agent template → scale_check count
	NamedSessionDemand map[string]bool // named-session identity → routed/assigned work demand
	WorkSet            map[string]bool // agent template → work_query found pending work
	RunningSessions    map[string]bool // session name → tmux exists
	AttachedSessions   map[string]bool // session name → user attached
	PendingSessions    map[string]bool // session name → pending interaction
	ReadyWaitSet       map[string]bool // session bead ID → durable wait is ready
	ChatIdleTimeout    time.Duration   // global idle timeout for manual/chat sessions (0 = disabled)
	Now                time.Time
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
	Identity    string // qualified name, e.g. "hello-world/refinery"
	Template    string // agent template name
	Mode        string // "always" or "on_demand"
	RuntimeName string // computed runtime session_name (e.g. "hello-world--refinery")
}

// AwakeSessionBead represents an open session bead from the store.
type AwakeSessionBead struct {
	ID                     string
	SessionName            string
	Template               string
	State                  string // "creating", "active", "asleep", "drained", "closed"
	SleepReason            string
	ManualSession          bool
	PendingCreate          bool      // controller claimed this bead for initial start
	DependencyOnly         bool      // only wakeable via dependency gate
	NamedIdentity          string    // non-empty for named session beads
	ConfiguredNamedSession bool      // configured_named_session metadata is true
	Pinned                 bool      // pin_awake durable wake reason
	Drained                bool      // state=="drained" or sleep_reason=="drained"
	WaitHold               bool      // user-issued gc wait in progress
	HeldUntil              time.Time // zero = not held
	QuarantinedUntil       time.Time // zero = not quarantined
	IdleSince              time.Time // zero = unknown/not idle
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
	// Drained beads are excluded from generic template demand, but explicit
	// compatible wake causes (pending create, named-always, assigned work) may
	// still reuse the same bead.
	desired := make(map[string]string) // sessionName → reason
	concreteAssignedWork := make(map[string]bool)

	// Newly created beads that still carry a controller create claim must be
	// launched at least once, even if the work signal that materialized them
	// is no longer visible on the very next tick.
	for _, bead := range input.SessionBeads {
		if !bead.PendingCreate {
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
			if sn := resolveNamedSessionBeadName(input.SessionBeads, ns); sn != "" {
				bead := findBeadBySessionName(input.SessionBeads, sn)
				if bead != nil && !bead.DependencyOnly {
					desired[sn] = "named-always"
				}
			} else {
				desired[ns.Identity] = "named-always"
			}
		case "on_demand":
			// On-demand named sessions wake only from named demand that was
			// resolved by the desired-state pass, not generic template demand.
			if !input.NamedSessionDemand[ns.Identity] {
				continue
			}
			if agent, ok := agentsByName[ns.Template]; ok && agent.Suspended {
				continue
			}
			if sn := resolveNamedSessionBeadName(input.SessionBeads, ns); sn != "" {
				bead := findBeadBySessionName(input.SessionBeads, sn)
				if bead != nil && !bead.DependencyOnly && !bead.Drained && bead.State != "closed" {
					desired[sn] = "named-demand"
				}
			} else {
				desired[ns.Identity] = "named-demand"
			}
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
		filled := 0
		for _, bead := range active {
			if filled >= count {
				break
			}
			if sessionHasConcreteAssignedWork(input.WorkBeads, bead) {
				continue
			}
			desired[bead.SessionName] = "scaled:demand"
			filled++
		}
		creating := collectCreatingBeads(input.SessionBeads, template)
		for _, bead := range creating {
			if filled >= count {
				break
			}
			if sessionHasConcreteAssignedWork(input.WorkBeads, bead) {
				continue
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
		if bead.State == "closed" {
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
			if assignee == bead.ID || assignee == bead.SessionName {
				desired[bead.SessionName] = "assigned-work"
				concreteAssignedWork[bead.SessionName] = true
				break
			}
			if bead.NamedIdentity != "" && assignee == bead.NamedIdentity {
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
		pinBlockedByState := bead.State == "suspended" || bead.State == "closed" || bead.Drained
		if !decision.ShouldWake && bead.Pinned && !pinBlockedByState && !bead.DependencyOnly && !bead.WaitHold {
			if agent, ok := agentsByName[bead.Template]; ok && !agent.Suspended {
				decision.ShouldWake = true
				decision.Reason = "pin"
			}
		}

		// Idle sleep: desired sessions idle too long should sleep.
		// Attached, pinned, and mode=always named sessions are exempt.
		if decision.ShouldWake && !input.AttachedSessions[name] && !input.PendingSessions[name] && !bead.Pinned && !bead.IdleSince.IsZero() &&
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
			if idleTimeout > 0 && input.Now.Sub(bead.IdleSince) >= idleTimeout && !concreteAssignedWork[name] {
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

// resolveNamedSessionBeadName locates the session_name of the bead that
// represents a configured named session. The primary match is the bead's
// NamedIdentity (configured_named_identity metadata). The fallback matches
// a configured_named_session bead by its session_name when that matches the
// identity's deterministic runtime name AND its template matches.
//
// The fallback recovers a configured named session whose NamedIdentity is
// MISSING on its bead — for example, a bead minted before
// configured_named_identity was added. Beads with a non-empty NamedIdentity
// that doesn't match any [[named_session]] identity are NOT recovered by
// this fallback (those return "" at the bead.NamedIdentity != ns.Identity
// check below); a config-change migration that leaves a stale NamedIdentity
// must be handled separately. Without this fallback, ComputeAwakeSet
// silently drops the bead from `desired` and the session stays asleep
// forever even though the config says mode=always. See #1493.
func resolveNamedSessionBeadName(beads []AwakeSessionBead, ns AwakeNamedSession) string {
	if sn := findNamedSessionName(beads, ns.Identity); sn != "" {
		return sn
	}
	if ns.RuntimeName == "" {
		return ""
	}
	bead := findBeadBySessionName(beads, ns.RuntimeName)
	if bead == nil || !bead.ConfiguredNamedSession {
		return ""
	}
	if ns.Template != "" && bead.Template != ns.Template {
		return ""
	}
	if bead.NamedIdentity != "" && bead.NamedIdentity != ns.Identity {
		return ""
	}
	return bead.SessionName
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
		// Exclude both NamedIdentity-tagged beads AND ConfiguredNamedSession
		// beads whose NamedIdentity happens to be missing — the latter are
		// still configured named sessions (recovered via the runtime-name
		// fallback in namedSessionMatches / resolveNamedSessionBeadName).
		// Treating them as generic pool candidates would re-introduce the
		// #1493 failure mode in a different shape: a configured named
		// session getting woken by generic template scale_check demand.
		if b.Template == template && b.State == "active" &&
			b.NamedIdentity == "" && !b.ConfiguredNamedSession &&
			!b.ManualSession && !b.Drained && !b.DependencyOnly {
			result = append(result, b)
		}
	}
	return result
}

func sessionHasConcreteAssignedWork(workBeads []AwakeWorkBead, bead AwakeSessionBead) bool {
	for _, wb := range workBeads {
		assignee := strings.TrimSpace(wb.Assignee)
		if assignee == "" || (wb.Status != "open" && wb.Status != "in_progress") {
			continue
		}
		if assignee == bead.ID || assignee == bead.SessionName {
			return true
		}
	}
	return false
}

func isOnDemandSession(named []AwakeNamedSession, bead AwakeSessionBead) bool {
	return namedSessionMatches(named, bead, "on_demand")
}

func isAlwaysNamedSession(named []AwakeNamedSession, bead AwakeSessionBead) bool {
	return namedSessionMatches(named, bead, "always")
}

// namedSessionMatches reports whether bead represents a configured named
// session of the given mode. The fallback path (bead.ConfiguredNamedSession
// + matching runtime name + template) mirrors resolveNamedSessionBeadName
// so a bead with missing/stale NamedIdentity is still recognized as named —
// otherwise idle-sleep suppression and on-demand keep-awake silently lose
// their exemption for affected beads. See #1493.
func namedSessionMatches(named []AwakeNamedSession, bead AwakeSessionBead, mode string) bool {
	for _, ns := range named {
		if ns.Mode != mode {
			continue
		}
		if bead.NamedIdentity != "" && ns.Identity == bead.NamedIdentity {
			return true
		}
		if bead.NamedIdentity == "" && bead.ConfiguredNamedSession &&
			ns.RuntimeName != "" && ns.RuntimeName == bead.SessionName &&
			(ns.Template == "" || ns.Template == bead.Template) {
			return true
		}
	}
	return false
}

func collectCreatingBeads(beads []AwakeSessionBead, template string) []AwakeSessionBead {
	var result []AwakeSessionBead
	for _, b := range beads {
		// See collectActiveBeads above for why ConfiguredNamedSession beads
		// must be excluded even when NamedIdentity is empty.
		if b.Template == template && b.State == "creating" &&
			b.NamedIdentity == "" && !b.ConfiguredNamedSession &&
			!b.ManualSession && !b.Drained && !b.DependencyOnly {
			result = append(result, b)
		}
	}
	return result
}
