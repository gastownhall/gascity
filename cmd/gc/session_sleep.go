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

// resolveSessionSleepPolicyInfo reads the session template and session name off
// Info (normalizedSessionTemplateInfo, Info.SessionNameMetadata — the RAW
// session_name, matching the raw form's session.Metadata["session_name"] read)
// and keeps the runtime capability probe (resolveSleepCapability) exactly as-is
// (§7 live edge). Everything else — the config policy resolution, the
// capability-downgrade switch, and the fingerprint — is computed from
// cfg/agent/sp and is byte-identical to the bead form.
func resolveSessionSleepPolicyInfo(info sessionpkg.Info, cfg *config.City, sp runtime.Provider) resolvedSessionSleepPolicy {
	agent := findAgentByTemplate(cfg, normalizedSessionTemplateInfo(info, cfg))
	resolved := config.ResolveSessionSleepPolicy(cfg, agent)
	policy := resolvedSessionSleepPolicy{
		Class:      resolved.Class,
		Requested:  resolved.Value,
		Effective:  resolved.Value,
		Source:     resolved.Source,
		Capability: resolveSleepCapability(sp, info.SessionNameMetadata),
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

// pendingInteractionKeepsAwakeInfo keeps the runtime probe
// (pendingInteractionReady) raw (§7 live edge), reads wait_hold off
// Info.WaitHold (trimmed), and feeds the lifecycle projection from
// LifecycleInputFromInfo — the projection consults only the held/quarantine
// timers here. It is the reconciler's pending-interaction deferral read (config
// drift drain, max-age kill, idle kill); no raw-bead form remains.
func pendingInteractionKeepsAwakeInfo(info sessionpkg.Info, sp runtime.Provider, name string, clk clock.Clock) bool {
	if !pendingInteractionReady(sp, name) {
		return false
	}
	if strings.TrimSpace(info.WaitHold) != "" {
		return false
	}
	var now time.Time
	if clk != nil {
		now = clk.Now()
	}
	lcInput := sessionpkg.LifecycleInputFromInfo(info)
	lcInput.Runtime = sessionpkg.RuntimeFacts{
		Observed: true,
		Alive:    true,
		Pending:  true,
	}
	lcInput.Now = now
	view := sessionpkg.ProjectLifecycle(lcInput)
	return !view.HasBlocker(sessionpkg.BlockerHeld) && !view.HasBlocker(sessionpkg.BlockerQuarantined)
}

// reconcileDetachedAtInfo tracks when a session last became detached for
// idle-sleep accounting. It reads detached_at off Info.DetachedAt and the
// session name off
// Info.SessionNameMetadata, keeps the runtime attach probe
// (workerSessionTargetAttachedWithConfig) and the detached_at write
// (sessFront.SetMarker keyed by Info.ID) exactly as the raw form does (§7 live
// edge), and returns the {"detached_at": <value>} batch for the reconciler to
// fold onto infoByID (nil on no-op). No raw-bead mirror.
func reconcileDetachedAtInfo(
	info sessionpkg.Info,
	store beads.Store,
	policy resolvedSessionSleepPolicy,
	alive bool,
	sp runtime.Provider,
	clk clock.Clock,
) map[string]string {
	if store == nil {
		return nil
	}
	if policy.Class == config.SessionSleepNonInteractive || !policy.enabled() || sp == nil || !alive || policy.Capability != runtime.SessionSleepCapabilityFull {
		if info.DetachedAt != "" {
			if err := sessionFrontDoor(store).SetMarker(info.ID, "detached_at", ""); err != nil {
				log.Printf("session sleep: clearing detached_at for %s: %v", info.ID, err)
			} else {
				return map[string]string{"detached_at": ""}
			}
		}
		return nil
	}
	name := info.SessionNameMetadata
	if name == "" {
		return nil
	}
	attached, err := workerSessionTargetAttachedWithConfig("", store, sp, nil, info.ID)
	if err == nil && attached {
		if info.DetachedAt != "" {
			if err := sessionFrontDoor(store).SetMarker(info.ID, "detached_at", ""); err != nil {
				log.Printf("session sleep: clearing detached_at for %s: %v", info.ID, err)
			} else {
				return map[string]string{"detached_at": ""}
			}
		}
		return nil
	}
	if info.DetachedAt == "" {
		ts := clk.Now().UTC().Format(time.RFC3339)
		if err := sessionFrontDoor(store).SetMarker(info.ID, "detached_at", ts); err != nil {
			log.Printf("session sleep: setting detached_at for %s: %v", info.ID, err)
		} else {
			return map[string]string{"detached_at": ts}
		}
	}
	return nil
}

// sessionIdleReferenceInfo reads detached_at off Info.DetachedAt (raw RFC3339)
// and the session name off Info.SessionNameMetadata (raw), and keeps the runtime
// last-activity probe (workerSessionTargetLastActivityWithConfig) as-is (§7 live edge).
func sessionIdleReferenceInfo(info sessionpkg.Info, sp runtime.Provider) time.Time {
	var detachedAt time.Time
	if raw := info.DetachedAt; raw != "" {
		detachedAt, _ = time.Parse(time.RFC3339, raw)
	}
	lastActivity := time.Time{}
	if sp != nil {
		if activity, err := workerSessionTargetLastActivityWithConfig("", nil, sp, nil, info.SessionNameMetadata); err == nil {
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

// configWakeSuppressedInfo is the session.Info sibling of configWakeSuppressed.
// It reads sleep_reason and sleep_policy_fingerprint off Info (the raw mirrors
// Info.SleepReason / Info.SleepPolicyFingerprint) and routes the idle reference
// through sessionIdleReferenceInfo. The fingerprint compare is exact against the
// freshly-resolved policy.Fingerprint, identical to the bead form.
func configWakeSuppressedInfo(
	info sessionpkg.Info,
	policy resolvedSessionSleepPolicy,
	sp runtime.Provider,
	clk clock.Clock,
) bool {
	if !policy.enabled() {
		return false
	}
	if info.SleepReason == string(sessionpkg.SleepReasonIdleTimeout) {
		return false
	}
	if info.SleepReason == string(sessionpkg.SleepReasonIdle) &&
		info.SleepPolicyFingerprint != "" &&
		info.SleepPolicyFingerprint == policy.Fingerprint {
		return true
	}
	if policy.Duration == 0 {
		return true
	}
	idleReference := sessionIdleReferenceInfo(info, sp)
	if idleReference.IsZero() {
		return false
	}
	return !clk.Now().Before(idleReference.Add(policy.Duration))
}

// sessionKeepWarmEligibleInfo routes the idle-reference read through
// sessionIdleReferenceInfo and the suppression check through
// configWakeSuppressedInfo.
func sessionKeepWarmEligibleInfo(
	info sessionpkg.Info,
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
	if sessionIdleReferenceInfo(info, sp).IsZero() {
		return false
	}
	return !configWakeSuppressedInfo(info, policy, sp, clk)
}

// persistSleepPolicyMetadataInfo reads the fingerprint-preservation state off
// Info (MetadataState == "asleep", SleepReason == "idle", SleepIntent ==
// "idle-stop-pending", SleepPolicyFingerprint) and diffs the seven policy keys
// against their raw Info mirrors, then folds any change through ApplyPatchInfo,
// returning the refreshed snapshot Info. The no-op-on-error swallow contract:
// ApplyPatchInfo returns the INPUT Info unchanged on a persist error (and on an
// empty diff), so a rejected write never advances the snapshot.
func persistSleepPolicyMetadataInfo(
	info sessionpkg.Info,
	sessFront *sessionpkg.Store,
	policy resolvedSessionSleepPolicy,
	configSuppressed bool,
) sessionpkg.Info {
	if sessFront == nil {
		return info
	}
	fingerprint := policy.Fingerprint
	if ((info.MetadataState == "asleep" &&
		info.SleepReason == string(sessionpkg.SleepReasonIdle)) ||
		info.SleepIntent == "idle-stop-pending") &&
		info.SleepPolicyFingerprint != "" {
		// Preserve the fingerprint that initiated an in-flight idle drain (same
		// reasoning as the raw form: config changes while running are handled by
		// wake evaluation before the drain completes).
		fingerprint = info.SleepPolicyFingerprint
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
	current := map[string]string{
		"requested_sleep_after_idle":     info.RequestedSleepAfterIdle,
		"effective_sleep_after_idle":     info.EffectiveSleepAfterIdle,
		"sleep_policy_source":            info.SleepPolicySource,
		"sleep_capability":               info.SleepCapability,
		"sleep_policy_adjustment_reason": info.SleepPolicyAdjustmentReason,
		"sleep_policy_fingerprint":       info.SleepPolicyFingerprint,
		"config_wake_suppressed":         info.ConfigWakeSuppressedMetadata,
	}
	changed := make(sessionpkg.MetadataPatch)
	for key, value := range batch {
		if current[key] != value {
			changed[key] = value
		}
	}
	if len(changed) == 0 {
		return info
	}
	next, _ := sessFront.ApplyPatchInfo(info, changed)
	return next
}

// markIdleSleepPendingInfo reads sleep_intent off Info.SleepIntent and the handle off Info.ID, writes
// the intent marker via sessFront.SetMarker, and returns the patch for the
// reconciler to fold onto infoByID (nil on no-op). No raw-bead mirror: the awake
// scan's drain arm never appends to startCandidates, so the freshly-marked bead
// is not re-read this tick.
func markIdleSleepPendingInfo(info sessionpkg.Info, sessFront *sessionpkg.Store) sessionpkg.MetadataPatch {
	if sessFront == nil || info.SleepIntent == "idle-stop-pending" {
		return nil
	}
	if err := sessFront.SetMarker(info.ID, "sleep_intent", "idle-stop-pending"); err != nil {
		return nil
	}
	return sessionpkg.MetadataPatch{"sleep_intent": "idle-stop-pending"}
}

// recoverPendingIdleSleepInfo reads the idle-stop-pending intent and the
// preserved fingerprint off Info (SleepIntent, SleepPolicyFingerprint), the
// handle off Info.ID, and persists SleepPatch(now, "idle") via
// sessFront.ApplyPatch. It returns only the bool: the caller reconstructs the
// time-independent SleepPatch fold onto infoByID (slept_at / fingerprint are
// non-Info), exactly as with the raw form. No raw-bead mirror.
func recoverPendingIdleSleepInfo(
	info sessionpkg.Info,
	sessFront *sessionpkg.Store,
	running bool,
	clk clock.Clock,
) bool {
	if sessFront == nil || running || info.SleepIntent != "idle-stop-pending" {
		return false
	}
	batch := sessionpkg.SleepPatch(clk.Now(), string(sessionpkg.SleepReasonIdle))
	if fingerprint := info.SleepPolicyFingerprint; fingerprint != "" {
		batch["sleep_policy_fingerprint"] = fingerprint
	}
	if err := sessFront.ApplyPatch(info.ID, batch); err != nil {
		return false
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
