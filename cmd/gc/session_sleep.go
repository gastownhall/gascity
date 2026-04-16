package main

import (
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

type resolvedSessionSleepPolicy struct {
	Class            config.SessionSleepClass
	Requested        string
	Effective        string
	Source           string
	Capability       runtime.SessionSleepCapability
	AdjustmentReason string
	Fingerprint      string
	Duration         time.Duration
}

const idleSleepProbeTimeout = time.Second

func (p resolvedSessionSleepPolicy) enabled() bool {
	return p.Effective != "" && p.Effective != config.SessionSleepOff
}

func resolveSessionSleepPolicy(session beads.Bead, cfg *config.City, sp runtime.Provider) resolvedSessionSleepPolicy {
	agent := findAgentByTemplate(cfg, normalizedSessionTemplate(session, cfg))
	resolved := config.ResolveSessionSleepPolicy(cfg, agent)
	policy := resolvedSessionSleepPolicy{
		Class:      resolved.Class,
		Requested:  resolved.Value,
		Effective:  resolved.Value,
		Source:     resolved.Source,
		Capability: resolveSleepCapability(sp, session.Metadata["session_name"]),
	}
	switch {
	case policy.Capability == runtime.SessionSleepCapabilityDisabled:
		policy.Effective = config.SessionSleepOff
		if resolved.Value != config.SessionSleepOff {
			policy.AdjustmentReason = "capability_disabled"
		}
	case policy.Class != config.SessionSleepNonInteractive && policy.Capability != runtime.SessionSleepCapabilityFull:
		policy.Effective = config.SessionSleepOff
		if resolved.Value != config.SessionSleepOff {
			policy.AdjustmentReason = "interactive_capability_insufficient"
		}
	}
	if duration, off, err := config.ParseSleepAfterIdle(policy.Effective); err == nil && !off {
		policy.Duration = duration
	}
	policy.Fingerprint = sessionSleepFingerprint(agent, policy)
	return policy
}

func resolveSleepCapability(sp runtime.Provider, name string) runtime.SessionSleepCapability {
	if sp == nil || name == "" {
		return runtime.SessionSleepCapabilityDisabled
	}
	if scp, ok := sp.(runtime.SleepCapabilityProvider); ok {
		if capability := scp.SleepCapability(name); capability != "" {
			return capability
		}
	}
	caps := sp.Capabilities()
	switch {
	case caps.CanReportActivity && caps.CanReportAttachment:
		return runtime.SessionSleepCapabilityFull
	case caps.CanReportActivity:
		return runtime.SessionSleepCapabilityTimedOnly
	default:
		return runtime.SessionSleepCapabilityDisabled
	}
}

func sessionSleepFingerprint(agent *config.Agent, policy resolvedSessionSleepPolicy) string {
	if agent == nil {
		return ""
	}
	return strings.Join([]string{
		"value=" + policy.Effective,
		"class=" + string(policy.Class),
		"source=" + policy.Source,
		"wake=" + agent.EffectiveWakeMode(),
		"cap=" + string(policy.Capability),
		"deps=" + strings.Join(agent.DependsOn, ","),
		"template=" + agent.QualifiedName(),
	}, "|")
}

func pendingInteractionReady(sp runtime.Provider, name string) bool {
	if sp == nil || name == "" {
		return false
	}
	ip, ok := sp.(runtime.InteractionProvider)
	if !ok {
		return false
	}
	pending, err := ip.Pending(name)
	if err != nil {
		return false
	}
	return pending != nil
}

func pendingInteractionKeepsAwake(session beads.Bead, sp runtime.Provider, name string, clk clock.Clock) bool {
	if !pendingInteractionReady(sp, name) {
		return false
	}
	if strings.TrimSpace(session.Metadata["wait_hold"]) != "" {
		return false
	}
	var now time.Time
	if clk != nil {
		now = clk.Now()
	}
	view := sessionpkg.ProjectLifecycle(sessionpkg.LifecycleInput{
		Status:   session.Status,
		Metadata: session.Metadata,
		Runtime: sessionpkg.RuntimeFacts{
			Observed: true,
			Alive:    true,
			Pending:  true,
		},
		Now: now,
	})
	return !view.HasBlocker(sessionpkg.BlockerHeld) && !view.HasBlocker(sessionpkg.BlockerQuarantined)
}

func reconcileDetachedAt(
	session *beads.Bead,
	store beads.Store,
	policy resolvedSessionSleepPolicy,
	alive bool,
	sp runtime.Provider,
	clk clock.Clock,
) {
	if session == nil || store == nil {
		return
	}
	if policy.Class == config.SessionSleepNonInteractive || !policy.enabled() || sp == nil || !alive || policy.Capability != runtime.SessionSleepCapabilityFull {
		if session.Metadata["detached_at"] != "" {
			_ = store.SetMetadata(session.ID, "detached_at", "")
			session.Metadata["detached_at"] = ""
		}
		return
	}
	name := session.Metadata["session_name"]
	if name == "" {
		return
	}
	if sp.IsAttached(name) {
		if session.Metadata["detached_at"] != "" {
			_ = store.SetMetadata(session.ID, "detached_at", "")
			session.Metadata["detached_at"] = ""
		}
		return
	}
	if session.Metadata["detached_at"] == "" {
		ts := clk.Now().UTC().Format(time.RFC3339)
		_ = store.SetMetadata(session.ID, "detached_at", ts)
		session.Metadata["detached_at"] = ts
	}
}

func sessionIdleReference(session beads.Bead, sp runtime.Provider) time.Time {
	var detachedAt time.Time
	if raw := session.Metadata["detached_at"]; raw != "" {
		detachedAt, _ = time.Parse(time.RFC3339, raw)
	}
	lastActivity := time.Time{}
	if sp != nil {
		if activity, err := sp.GetLastActivity(session.Metadata["session_name"]); err == nil {
			lastActivity = activity
		}
	}
	switch {
	case detachedAt.IsZero():
		return lastActivity
	case lastActivity.IsZero():
		return detachedAt
	case lastActivity.After(detachedAt):
		return lastActivity
	default:
		return detachedAt
	}
}

func configWakeSuppressed(
	session beads.Bead,
	policy resolvedSessionSleepPolicy,
	sp runtime.Provider,
	clk clock.Clock,
) bool {
	if !policy.enabled() {
		return false
	}
	if session.Metadata["sleep_reason"] == "idle-timeout" {
		return false
	}
	if session.Metadata["sleep_reason"] == "idle" &&
		session.Metadata["sleep_policy_fingerprint"] != "" &&
		session.Metadata["sleep_policy_fingerprint"] == policy.Fingerprint {
		return true
	}
	if policy.Duration == 0 {
		return true
	}
	idleReference := sessionIdleReference(session, sp)
	if idleReference.IsZero() {
		return false
	}
	return !clk.Now().Before(idleReference.Add(policy.Duration))
}

func sessionKeepWarmEligible(
	session beads.Bead,
	policy resolvedSessionSleepPolicy,
	sp runtime.Provider,
	clk clock.Clock,
) bool {
	if !policy.enabled() || policy.Class == config.SessionSleepNonInteractive {
		return false
	}
	if policy.Duration == 0 {
		return false
	}
	if sessionIdleReference(session, sp).IsZero() {
		return false
	}
	return !configWakeSuppressed(session, policy, sp, clk)
}

func persistSleepPolicyMetadata(
	session *beads.Bead,
	store beads.Store,
	policy resolvedSessionSleepPolicy,
	configSuppressed bool,
) {
	if session == nil || store == nil {
		return
	}
	fingerprint := policy.Fingerprint
	if ((session.Metadata["state"] == "asleep" &&
		session.Metadata["sleep_reason"] == "idle") ||
		session.Metadata["sleep_intent"] == "idle-stop-pending") &&
		session.Metadata["sleep_policy_fingerprint"] != "" {
		// Preserve the fingerprint that initiated an in-flight idle drain so the
		// eventual asleep state remains tied to the policy that actually put the
		// session to sleep. Config changes while the session is still running are
		// handled by wake evaluation before the drain completes.
		fingerprint = session.Metadata["sleep_policy_fingerprint"]
	}
	batch := map[string]string{
		"requested_sleep_after_idle":     policy.Requested,
		"effective_sleep_after_idle":     policy.Effective,
		"sleep_policy_source":            policy.Source,
		"sleep_capability":               string(policy.Capability),
		"sleep_policy_adjustment_reason": policy.AdjustmentReason,
		"sleep_policy_fingerprint":       fingerprint,
		"config_wake_suppressed":         boolMetadata(configSuppressed),
	}
	changed := make(map[string]string)
	for key, value := range batch {
		if session.Metadata[key] != value {
			changed[key] = value
		}
	}
	if len(changed) == 0 {
		return
	}
	if err := store.SetMetadataBatch(session.ID, changed); err != nil {
		return
	}
	if session.Metadata == nil {
		session.Metadata = make(map[string]string, len(changed))
	}
	for key, value := range changed {
		session.Metadata[key] = value
	}
}

func markIdleSleepPending(session *beads.Bead, store beads.Store) {
	if session == nil || store == nil || session.Metadata["sleep_intent"] == "idle-stop-pending" {
		return
	}
	if err := store.SetMetadata(session.ID, "sleep_intent", "idle-stop-pending"); err != nil {
		return
	}
	if session.Metadata == nil {
		session.Metadata = make(map[string]string, 1)
	}
	session.Metadata["sleep_intent"] = "idle-stop-pending"
}

func recoverPendingIdleSleep(
	session *beads.Bead,
	store beads.Store,
	running bool,
	clk clock.Clock,
) bool {
	if session == nil || store == nil || running || session.Metadata["sleep_intent"] != "idle-stop-pending" {
		return false
	}
	batch := sessionpkg.SleepPatch(clk.Now(), "idle")
	if fingerprint := session.Metadata["sleep_policy_fingerprint"]; fingerprint != "" {
		batch["sleep_policy_fingerprint"] = fingerprint
	}
	if err := store.SetMetadataBatch(session.ID, batch); err != nil {
		return false
	}
	if session.Metadata == nil {
		session.Metadata = make(map[string]string, len(batch))
	}
	for key, value := range batch {
		session.Metadata[key] = value
	}
	return true
}

func boolMetadata(v bool) string {
	if v {
		return "true"
	}
	return ""
}

func isManualSessionBead(bead beads.Bead) bool {
	return strings.TrimSpace(bead.Metadata["session_origin"]) == "manual" || bead.Metadata["manual_session"] == boolMetadata(true)
}
