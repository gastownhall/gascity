package main

import (
	"log"
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

func sessionActivityReportable(sp runtime.Provider, name string) bool {
	if sp == nil || name == "" {
		return false
	}
	sleepCapability := resolveSleepCapability(sp, name)
	return sleepCapability != runtime.SessionSleepCapabilityDisabled &&
		(sleepCapability != runtime.SessionSleepCapabilityTimedOnly || sp.Capabilities().CanReportActivity)
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
	if cached, ok := sp.(*attachmentCachingProvider); ok && cached.Provider != nil {
		sp = cached.Provider
	}
	pending, err := workerSessionTargetPendingWithConfig("", nil, sp, nil, name)
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
	lcInput := sessionpkg.LifecycleInputFromMetadata(session.Status, session.Metadata)
	lcInput.Runtime = sessionpkg.RuntimeFacts{
		Observed: true,
		Alive:    true,
		Pending:  true,
	}
	lcInput.Now = now
	view := sessionpkg.ProjectLifecycle(lcInput)
	return !view.HasBlocker(sessionpkg.BlockerHeld) && !view.HasBlocker(sessionpkg.BlockerQuarantined)
}

// reconcileDetachedAt tracks when a session last became detached for idle-sleep
// accounting. Returns the mirrored {"detached_at": <value>} batch when a write
// is persisted (clear or set), nil when no change is made. The caller folds the
// returned batch onto the typed snapshot via ApplyPatch (nil is a no-op).
func reconcileDetachedAt(
	session *beads.Bead,
	store beads.Store,
	policy resolvedSessionSleepPolicy,
	alive bool,
	sp runtime.Provider,
	clk clock.Clock,
) map[string]string {
	if session == nil || store == nil {
		return nil
	}
	if policy.Class == config.SessionSleepNonInteractive || !policy.enabled() || sp == nil || !alive || policy.Capability != runtime.SessionSleepCapabilityFull {
		if session.Metadata["detached_at"] != "" {
			if err := sessionFrontDoor(store).SetMarker(session.ID, "detached_at", ""); err != nil {
				log.Printf("session sleep: clearing detached_at for %s: %v", session.ID, err)
			} else {
				session.Metadata["detached_at"] = ""
				return map[string]string{"detached_at": ""}
			}
		}
		return nil
	}
	name := session.Metadata["session_name"]
	if name == "" {
		return nil
	}
	attached, err := workerSessionTargetAttachedWithConfig("", store, sp, nil, session.ID)
	if err == nil && attached {
		if session.Metadata["detached_at"] != "" {
			if err := sessionFrontDoor(store).SetMarker(session.ID, "detached_at", ""); err != nil {
				log.Printf("session sleep: clearing detached_at for %s: %v", session.ID, err)
			} else {
				session.Metadata["detached_at"] = ""
				return map[string]string{"detached_at": ""}
			}
		}
		return nil
	}
	if session.Metadata["detached_at"] == "" {
		ts := clk.Now().UTC().Format(time.RFC3339)
		if err := sessionFrontDoor(store).SetMarker(session.ID, "detached_at", ts); err != nil {
			log.Printf("session sleep: setting detached_at for %s: %v", session.ID, err)
		} else {
			session.Metadata["detached_at"] = ts
			return map[string]string{"detached_at": ts}
		}
	}
	return nil
}

func sessionIdleReference(session beads.Bead, sp runtime.Provider) time.Time {
	var detachedAt time.Time
	if raw := session.Metadata["detached_at"]; raw != "" {
		detachedAt, _ = time.Parse(time.RFC3339, raw)
	}
	lastActivity := time.Time{}
	if sp != nil {
		if activity, err := workerSessionTargetLastActivityWithConfig("", nil, sp, nil, session.Metadata["session_name"]); err == nil {
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
	if session.Metadata["sleep_reason"] == string(sessionpkg.SleepReasonIdleTimeout) {
		return false
	}
	if session.Metadata["sleep_reason"] == string(sessionpkg.SleepReasonIdle) &&
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
	sessFront *sessionpkg.Store,
	policy resolvedSessionSleepPolicy,
	configSuppressed bool,
) {
	if session == nil || sessFront == nil {
		return
	}
	fingerprint := policy.Fingerprint
	if ((session.Metadata["state"] == "asleep" &&
		session.Metadata["sleep_reason"] == string(sessionpkg.SleepReasonIdle)) ||
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
	if err := sessFront.ApplyPatch(session.ID, changed); err != nil {
		return
	}
	if session.Metadata == nil {
		session.Metadata = make(map[string]string, len(changed))
	}
	for key, value := range changed {
		session.Metadata[key] = value
	}
}

// markIdleSleepPending returns the metadata patch it applied (sleep_intent =
// idle-stop-pending) so the reconciler can fold it onto the infoByID snapshot
// (write-returns-Info), or nil when it was a no-op. The raw mirror onto
// session.Metadata is kept (dropped in Step 5).
func markIdleSleepPending(session *beads.Bead, sessFront *sessionpkg.Store) sessionpkg.MetadataPatch {
	if session == nil || sessFront == nil || session.Metadata["sleep_intent"] == "idle-stop-pending" {
		return nil
	}
	if err := sessFront.SetMarker(session.ID, "sleep_intent", "idle-stop-pending"); err != nil {
		return nil
	}
	if session.Metadata == nil {
		session.Metadata = make(map[string]string, 1)
	}
	session.Metadata["sleep_intent"] = "idle-stop-pending"
	return sessionpkg.MetadataPatch{"sleep_intent": "idle-stop-pending"}
}

func recoverPendingIdleSleep(
	session *beads.Bead,
	sessFront *sessionpkg.Store,
	running bool,
	clk clock.Clock,
) bool {
	if session == nil || sessFront == nil || running || session.Metadata["sleep_intent"] != "idle-stop-pending" {
		return false
	}
	batch := sessionpkg.SleepPatch(clk.Now(), string(sessionpkg.SleepReasonIdle))
	if fingerprint := session.Metadata["sleep_policy_fingerprint"]; fingerprint != "" {
		batch["sleep_policy_fingerprint"] = fingerprint
	}
	if err := sessFront.ApplyPatch(session.ID, batch); err != nil {
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

// isManualSessionInfo is the session.Info mirror of isManualSessionBead. The
// manual_session clause compares the RAW metadata (Info.ManualSessionMetadata)
// without trimming, exactly as the bead form does.
func isManualSessionInfo(i sessionpkg.Info) bool {
	return strings.TrimSpace(i.SessionOrigin) == "manual" || i.ManualSessionMetadata == boolMetadata(true)
}
